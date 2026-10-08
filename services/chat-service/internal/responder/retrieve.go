package responder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	chatpb "chat-service/chat"
	"chat-service/internal/guard"
	"chat-service/internal/obs"
	"chat-service/internal/rag"
)

type searcher interface {
	Search(ctx context.Context, vec []float32, limit int, kind string) ([]rag.Hit, error)
}

// grounder binds one turn's system prompt. RetrievingResponder depends on this
// rather than *OpenAIResponder so it is testable without a provider.
type grounder interface {
	withSystem(system string) Responder
}

type condenser interface {
	condense(ctx context.Context, msgs []*chatpb.Message) (string, error)
}

type warmer interface {
	warm(ctx context.Context) error
}

// RetrievingResponder grounds each turn in retrieved passages, then delegates.
// chatServer.Chat does not learn about Qdrant: this fills the same Responder
// seam the echo stub does.
type RetrievingResponder struct {
	Inner     grounder
	Embedder  rag.Embedder
	Store     searcher
	Condenser condenser
	Warm      warmer
	Guard     guard.Guard // nil: unguarded, as in tests that predate it
	TopK      int
	MinScore  float32
	Floor     int

	cache warmCache
}

func (r *RetrievingResponder) Stream(ctx context.Context, req *chatpb.ChatRequest, emit func(delta string) error) (Usage, error) {
	msgs := req.GetMessages()

	// Retrieval runs while the guard checks, so the guard costs no latency.
	groundCtx, cancelGround := context.WithCancel(ctx)
	defer cancelGround()
	type grounded struct {
		system string
		err    error
	}
	done := make(chan grounded, 1)
	go func() {
		system, err := r.ground(groundCtx, msgs)
		done <- grounded{system, err}
	}()

	if r.flagged(ctx, msgs) {
		cancelGround()
		if err := emit(guardRefusal); err != nil {
			return Usage{}, err
		}
		return Usage{StopReason: "guarded"}, nil
	}

	g := <-done
	system := g.system
	if g.err != nil {
		// Bare, so classifyOutcome sees a hangup rather than a dead provider.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Usage{}, ctxErr
		}
		obs.LogWithTrace(ctx, slog.Default()).Warn("retrieval unavailable, degrading the reply", "error", g.err)
		system = unavailableEnvelope
	}
	return r.Inner.withSystem(system).Stream(ctx, req, emit)
}

// flagged fails open: signing and the prompt still hold when the guard is down.
func (r *RetrievingResponder) flagged(ctx context.Context, msgs []*chatpb.Message) bool {
	if r.Guard == nil {
		return false
	}
	var texts []string
	for _, m := range msgs {
		if m.GetRole() == chatpb.Role_ROLE_USER {
			texts = append(texts, m.GetContent())
		}
	}
	v, err := r.Guard.Check(ctx, texts)
	log := obs.LogWithTrace(ctx, slog.Default())
	switch {
	case err != nil && ctx.Err() == nil:
		log.Warn("injection guard unavailable, continuing unguarded", "error", err)
	case v.Flagged:
		log.Info("injection guard flagged a turn", "score", v.Score)
	}
	return err == nil && v.Flagged
}

// Warmup readies both models a turn needs. The probe text is irrelevant;
// embedding anything loads the embedder.
func (r *RetrievingResponder) Warmup(ctx context.Context) error {
	return r.cache.do(ctx, func(ctx context.Context) error {
		if _, err := r.Embedder.Embed(ctx, []string{"warmup"}); err != nil {
			return fmt.Errorf("warming embedder: %w", err)
		}
		if err := r.Warm.warm(ctx); err != nil {
			return fmt.Errorf("warming model: %w", err)
		}
		return nil
	})
}

// ground builds one turn's system prompt. Every error path here is soft — the
// caller degrades — so it reports failures through the metrics and returns.
func (r *RetrievingResponder) ground(ctx context.Context, msgs []*chatpb.Message) (string, error) {
	query := r.query(ctx, msgs)

	embedStart := time.Now()
	vecs, err := r.Embedder.Embed(ctx, []string{query})
	chatEmbedDuration.Observe(time.Since(embedStart).Seconds())
	if err != nil {
		return "", retrievalError("embed_failed", err)
	}
	if len(vecs) != 1 {
		return "", retrievalError("decode_error", fmt.Errorf("embedder returned %d vectors for one query", len(vecs)))
	}

	searchStart := time.Now()
	hits, err := r.search(ctx, vecs[0])
	chatRetrievalDuration.Observe(time.Since(searchStart).Seconds())
	if err != nil {
		return "", retrievalError(searchReason(err), err)
	}

	chatRetrievalChunks.Observe(float64(len(hits)))
	if len(hits) == 0 {
		// Not an error. The envelope's "say so plainly" rule takes over, which
		// is the honest answer to a question the corpus cannot answer.
		return corpusEnvelope, nil
	}
	return corpusEnvelope + citationRule + "\n\n" + assemblePrompt(hits) + "\n" + sourcesReminder, nil
}

// query folds history into a standalone question. A follow-up carries its
// meaning in the history, not its own words.
func (r *RetrievingResponder) query(ctx context.Context, msgs []*chatpb.Message) string {
	raw := lastContent(msgs)
	if len(msgs) < 2 || r.Condenser == nil {
		return raw
	}

	start := time.Now()
	condensed, err := r.Condenser.condense(ctx, msgs)
	chatCondenseDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		// Degraded retrieval beats no answer.
		obs.LogWithTrace(ctx, slog.Default()).Warn("condensation failed, embedding the raw turn", "error", err)
		return raw
	}
	return condensed
}

// search runs the quota'd pair. Two searches rather than one because the floor
// has to be guaranteed, not hoped for.
func (r *RetrievingResponder) search(ctx context.Context, vec []float32) ([]rag.Hit, error) {
	floor, err := r.Store.Search(ctx, vec, r.Floor, "background")
	if err != nil {
		return nil, err
	}
	rest, err := r.Store.Search(ctx, vec, r.TopK, "")
	if err != nil {
		return nil, err
	}

	merged := append(floor, rest...)
	if len(merged) > 0 {
		// Before the threshold, or the histogram MIN_SCORE is tuned from can
		// never show how far below it the declined turns fell. The max, not
		// merged[0]: the list leads with the quota, which scores below the winner.
		chatRetrievalTopScore.Observe(float64(bestScore(merged)))
	}

	seen := make(map[string]bool, r.TopK)
	out := make([]rag.Hit, 0, r.TopK)
	for _, h := range merged {
		if seen[h.ID] || len(out) == r.TopK {
			continue
		}
		// The honesty floor: below it the corpus does not answer this, and the
		// envelope's decline is the correct reply.
		if h.Score < r.MinScore {
			continue
		}
		seen[h.ID] = true
		out = append(out, h)
	}
	return out, nil
}

func bestScore(hits []rag.Hit) float32 {
	best := hits[0].Score
	for _, h := range hits[1:] {
		if h.Score > best {
			best = h.Score
		}
	}
	return best
}

// sourcesTag matches the spotlight delimiters in any case or spacing, so a
// chunk cannot close the block early.
var sourcesTag = regexp.MustCompile(`(?i)<\s*/?\s*sources\s*>`)

// assemblePrompt numbers the surviving chunks. Numbering is positional, so the
// list handed to the model and the citations it is told to use cannot drift.
func assemblePrompt(hits []rag.Hit) string {
	var b strings.Builder
	b.WriteString("<sources>\nSources:\n")
	for i, h := range hits {
		line := fmt.Sprintf("  [%d] (%s) %s", i+1, citation(h.Payload), collapse(h.Payload.Text))
		b.WriteString(sourcesTag.ReplaceAllString(line, ""))
		b.WriteString("\n")
	}
	b.WriteString("</sources>\n")
	return b.String()
}

// citation is what a reader follows back to the file: a heading for prose, a
// line and symbol for code.
func citation(p rag.Payload) string {
	if p.Symbol != "" {
		return fmt.Sprintf("%s:%d %s", p.Source, p.Line, p.Symbol)
	}
	if p.Heading != "" {
		return fmt.Sprintf("%s § %s", p.Source, p.Heading)
	}
	return p.Source
}

// collapse keeps one chunk on one line: a bare newline in the middle of a
// numbered list reads to the model as the end of the list.
func collapse(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func retrievalError(reason string, err error) error {
	// A hung-up browser is not a retrieval failure. Counting it here would put
	// every cancelled stream in the error panel classifyOutcome keeps it out of.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	chatRetrievalErrorsTotal.WithLabelValues(reason).Inc()
	return err
}

// searchReason separates "start the container" from "rebuild the index" from
// "the response was malformed" — three different fixes.
func searchReason(err error) string {
	switch {
	case errors.Is(err, rag.ErrCollectionMissing):
		return "collection_missing"
	case strings.Contains(err.Error(), "decoding"):
		return "decode_error"
	default:
		return "unreachable"
	}
}
