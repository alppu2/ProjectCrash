package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	chatpb "chat-service/chat"
	"chat-service/internal/rag"
)

type fakeSearcher struct {
	byKind map[string][]rag.Hit
	err    error
	calls  []string
}

func (f *fakeSearcher) Search(_ context.Context, _ []float32, limit int, kind string) ([]rag.Hit, error) {
	f.calls = append(f.calls, kind)
	if f.err != nil {
		return nil, f.err
	}
	hits := f.byKind[kind]
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

type fakeEmbedder struct {
	err error
}

func (f fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0, 0, 0}
	}
	return out, nil
}
func (f fakeEmbedder) Dims() int       { return 4 }
func (f fakeEmbedder) ModelID() string { return "fake" }

// fakeGrounder captures the per-turn system prompt and replays a fixed reply.
type fakeGrounder struct{ system string }

func (g *fakeGrounder) withSystem(system string) Responder {
	g.system = system
	return &EchoResponder{}
}

type fakeCondenser struct {
	out  string
	err  error
	seen int
}

func (c *fakeCondenser) condense(_ context.Context, _ []*chatpb.Message) (string, error) {
	c.seen++
	return c.out, c.err
}

func hit(id, kind, source, text string, score float32) rag.Hit {
	return rag.Hit{ID: id, Score: score, Payload: rag.Payload{
		Kind: kind, Source: source, Text: text, Line: 17,
	}}
}

func userTurn(content string) []*chatpb.Message {
	return []*chatpb.Message{{Role: chatpb.Role_ROLE_USER, Content: content}}
}

func newTestRetriever(s *fakeSearcher, g *fakeGrounder, c *fakeCondenser) *RetrievingResponder {
	return &RetrievingResponder{
		Inner:     g,
		Embedder:  fakeEmbedder{},
		Store:     s,
		Condenser: c,
		TopK:      6,
		MinScore:  0.5,
		Floor:     2,
	}
}

func drain(t *testing.T, r Responder, msgs []*chatpb.Message) {
	t.Helper()
	if _, err := r.Stream(context.Background(), &chatpb.ChatRequest{Messages: msgs}, func(string) error { return nil }); err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
}

// "Who are you?" has no semantic anchor and retrieves arbitrarily. The quota
// is what keeps identity answerable without stuffing the background.
func TestRetrievalReservesBackgroundFloor(t *testing.T) {
	s := &fakeSearcher{byKind: map[string][]rag.Hit{
		"background": {
			hit("b1", "background", "corpus/background.md", "Aleksi is a backend engineer.", 0.62),
			hit("b2", "background", "corpus/background.md", "Based in Finland.", 0.58),
		},
		"": {
			hit("s1", "source", "chat-service/chat.go", "func Chat", 0.91),
			hit("s2", "source", "order-service/main.go", "func main", 0.90),
			hit("s3", "source", "envoy.yaml", "routes", 0.89),
			hit("s4", "source", "frontend/src/api.ts", "transport", 0.88),
			hit("s5", "source", "proto/chat.proto", "service", 0.87),
			hit("s6", "source", "inventory-service/main.go", "consume", 0.86),
		},
	}}
	g := &fakeGrounder{}
	drain(t, newTestRetriever(s, g, &fakeCondenser{}), userTurn("who are you?"))

	if !strings.Contains(g.system, "Aleksi is a backend engineer.") {
		t.Errorf("system prompt lost the background floor:\n%s", g.system)
	}
	if len(s.calls) != 2 || s.calls[0] != "background" || s.calls[1] != "" {
		t.Errorf("searches = %v, want a background search then an unfiltered one", s.calls)
	}
}

// The threshold is the honesty floor. Handing over the five least-bad chunks
// invites invention about a real person.
func TestRetrievalDropsBelowMinScore(t *testing.T) {
	s := &fakeSearcher{byKind: map[string][]rag.Hit{
		"background": {hit("b1", "background", "corpus/background.md", "irrelevant", 0.21)},
		"":           {hit("s1", "source", "chat-service/chat.go", "irrelevant too", 0.30)},
	}}
	g := &fakeGrounder{}
	drain(t, newTestRetriever(s, g, &fakeCondenser{}), userTurn("what is the capital of Peru?"))

	if strings.Contains(g.system, "Sources:") {
		t.Errorf("system prompt has a Sources block with everything below threshold:\n%s", g.system)
	}
	if !strings.Contains(g.system, corpusEnvelope) {
		t.Error("system prompt lost corpusEnvelope")
	}
}

// The same chunk can win both searches. A duplicate wastes a top_k slot and
// numbers the same passage twice.
func TestRetrievalDedupesByID(t *testing.T) {
	shared := hit("b1", "background", "corpus/background.md", "Backend engineer.", 0.80)
	s := &fakeSearcher{byKind: map[string][]rag.Hit{
		"background": {shared},
		"":           {shared, hit("s1", "source", "chat-service/chat.go", "func Chat", 0.75)},
	}}
	g := &fakeGrounder{}
	drain(t, newTestRetriever(s, g, &fakeCondenser{}), userTurn("tell me about the chat service"))

	if n := strings.Count(g.system, "Backend engineer."); n != 1 {
		t.Errorf("shared chunk appears %d times, want 1:\n%s", n, g.system)
	}
	if !strings.Contains(g.system, "[1]") || !strings.Contains(g.system, "[2]") {
		t.Errorf("sources are not numbered 1..n:\n%s", g.system)
	}
	if strings.Contains(g.system, "[3]") {
		t.Errorf("numbering ran past the deduped set:\n%s", g.system)
	}
}

// The histogram RETRIEVAL_MIN_SCORE gets tuned from must see the best score of
// the turn. The merged list leads with the background quota, whose scores are
// routinely lower than the unfiltered winner's.
func TestRetrievalObservesBestScoreNotFirst(t *testing.T) {
	s := &fakeSearcher{byKind: map[string][]rag.Hit{
		"background": {hit("b1", "background", "corpus/background.md", "Backend engineer.", 0.55)},
		"":           {hit("s1", "source", "chat-service/chat.go", "func Chat", 0.93)},
	}}
	before := histogramSum(t, chatRetrievalTopScore)
	drain(t, newTestRetriever(s, &fakeGrounder{}, &fakeCondenser{}), userTurn("how does the chat service stream?"))

	if got := histogramSum(t, chatRetrievalTopScore) - before; got < 0.92 || got > 0.94 {
		t.Errorf("observed top score = %v, want the best hit's 0.93, not the quota's 0.55", got)
	}
}

// Citations carry file and line for source, heading for markdown. A citation
// nobody can follow is not a citation.
func TestAssemblePromptCitationShape(t *testing.T) {
	got := assemblePrompt([]rag.Hit{
		{ID: "a", Payload: rag.Payload{Kind: "background", Source: "corpus/background.md", Heading: "Work history", Text: "Backend engineer."}},
		{ID: "b", Payload: rag.Payload{Kind: "source", Source: "order-service/amqp_carrier.go", Line: 17, Symbol: "amqpHeaderCarrier.Set", Text: "func Set"}},
	})

	if !strings.Contains(got, "[1] (corpus/background.md § Work history)") {
		t.Errorf("markdown citation malformed:\n%s", got)
	}
	if !strings.Contains(got, "[2] (order-service/amqp_carrier.go:17 amqpHeaderCarrier.Set)") {
		t.Errorf("source citation malformed:\n%s", got)
	}
}

// A follow-up carries its meaning in the history. Embedding "what about
// testing?" alone retrieves nothing useful.
func TestCondensationRunsOnlyWithHistory(t *testing.T) {
	s := &fakeSearcher{byKind: map[string][]rag.Hit{}}
	c := &fakeCondenser{out: "how is the chat service tested?"}
	drain(t, newTestRetriever(s, &fakeGrounder{}, c), userTurn("what about testing?"))
	if c.seen != 0 {
		t.Errorf("condenser ran %d times on a first turn, want 0", c.seen)
	}

	c2 := &fakeCondenser{out: "how is the chat service tested?"}
	drain(t, newTestRetriever(s, &fakeGrounder{}, c2), []*chatpb.Message{
		{Role: chatpb.Role_ROLE_USER, Content: "tell me about the chat service"},
		{Role: chatpb.Role_ROLE_ASSISTANT, Content: "It streams over gRPC-Web."},
		{Role: chatpb.Role_ROLE_USER, Content: "what about testing?"},
	})
	if c2.seen != 1 {
		t.Errorf("condenser ran %d times with history, want 1", c2.seen)
	}
}

// Degraded retrieval beats no answer: a condensation failure must not fail
// the turn.
func TestCondensationFailureFallsBackToRawTurn(t *testing.T) {
	s := &fakeSearcher{byKind: map[string][]rag.Hit{
		"": {hit("s1", "source", "chat-service/chat.go", "func Chat", 0.80)},
	}}
	g := &fakeGrounder{}
	c := &fakeCondenser{err: errors.New("model down")}

	drain(t, newTestRetriever(s, g, c), []*chatpb.Message{
		{Role: chatpb.Role_ROLE_USER, Content: "tell me about the chat service"},
		{Role: chatpb.Role_ROLE_ASSISTANT, Content: "It streams."},
		{Role: chatpb.Role_ROLE_USER, Content: "what about testing?"},
	})
	if !strings.Contains(g.system, "func Chat") {
		t.Errorf("fallback did not search at all:\n%s", g.system)
	}
}

// Qdrant blinking must not lose a conversation, but it must not let the model
// answer as though it still knows the corpus either.
func TestSearchFailureDegradesToUnavailablePrompt(t *testing.T) {
	s := &fakeSearcher{err: errors.New("connection refused")}
	g := &fakeGrounder{}
	drain(t, newTestRetriever(s, g, &fakeCondenser{}), userTurn("who are you?"))

	if !strings.Contains(g.system, unavailableEnvelope) {
		t.Errorf("system prompt did not degrade:\n%s", g.system)
	}
	if strings.Contains(g.system, "Sources:") {
		t.Errorf("degraded prompt still has a Sources block:\n%s", g.system)
	}
}

// classifyOutcome needs the bare context error, and a hung-up browser must not
// land in the retrieval error panel. Both regressions look identical in code
// review and only show up as a Grafana error spike under real traffic.
func TestCancellationIsReturnedBareAndUncounted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	before := errorCount(t, "unreachable")

	r := newTestRetriever(&fakeSearcher{err: ctx.Err()}, &fakeGrounder{}, &fakeCondenser{})
	_, err := r.Stream(ctx, &chatpb.ChatRequest{Messages: userTurn("hi")}, func(string) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Stream() error = %v, want a bare context.Canceled", err)
	}
	if classifyOutcome(err) != "cancelled" {
		t.Errorf("classifyOutcome = %q, want cancelled", classifyOutcome(err))
	}
	if got := errorCount(t, "unreachable"); got != before {
		t.Errorf("chat_retrieval_errors_total{reason=\"unreachable\"} = %v, want %v — a cancelled client is not a retrieval failure", got, before)
	}
}

func errorCount(t *testing.T, reason string) float64 {
	t.Helper()
	var m dto.Metric
	if err := chatRetrievalErrorsTotal.WithLabelValues(reason).(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("reading counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

func histogramSum(t *testing.T, h prometheus.Histogram) float64 {
	t.Helper()
	var m dto.Metric
	if err := h.Write(&m); err != nil {
		t.Fatalf("reading histogram: %v", err)
	}
	return m.GetHistogram().GetSampleSum()
}
