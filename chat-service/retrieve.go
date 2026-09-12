package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	chatpb "chat-service/chat"
	"chat-service/internal/rag"
)

// unavailableEnvelope replaces the grounding prompt when retrieval fails
// mid-stream. Degrade, don't die: the conversation survives, but the model is
// told it cannot answer rather than left to answer from pretraining.
const unavailableEnvelope = `You are the portfolio assistant on Aleksi Valta's engineering portfolio site.
Its knowledge base is temporarily unavailable, so you have no material to answer from.
Say plainly that you cannot look anything up right now and suggest trying again in a moment.
Do not answer from memory, and do not invent any detail about him.`

// citationRule is appended to corpusEnvelope when there is a Sources block to
// cite. Without sources it would instruct the model to cite nothing.
const citationRule = `
- Cite the bracketed number of the source each claim comes from, like [2].`

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

// RetrievingResponder grounds each turn in retrieved passages, then delegates.
// chatServer.Chat does not learn about Qdrant: this fills the same Responder
// seam the echo stub does.
type RetrievingResponder struct {
	Inner     grounder
	Embedder  rag.Embedder
	Store     searcher
	Condenser condenser
	TopK      int
	MinScore  float32
	Floor     int
}

func (r *RetrievingResponder) Stream(ctx context.Context, req *chatpb.ChatRequest, emit func(delta string) error) (Usage, error) {
	system, err := r.ground(ctx, req.GetMessages())
	if err != nil {
		// Bare, so classifyOutcome sees a hangup rather than a dead provider.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Usage{}, ctxErr
		}
		logWithTrace(ctx, slog.Default()).Warn("retrieval unavailable, degrading the reply", "error", err)
		system = unavailableEnvelope
	}
	return r.Inner.withSystem(system).Stream(ctx, req, emit)
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
	if len(hits) > 0 {
		// The best score of the turn, not hits[0]: the merged list leads with
		// the background quota, which routinely scores below the real winner.
		chatRetrievalTopScore.Observe(float64(bestScore(hits)))
	}
	if len(hits) == 0 {
		// Not an error. The envelope's "say so plainly" rule takes over, which
		// is the honest answer to a question the corpus cannot answer.
		return corpusEnvelope, nil
	}
	return corpusEnvelope + citationRule + "\n\n" + assemblePrompt(hits), nil
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
		logWithTrace(ctx, slog.Default()).Warn("condensation failed, embedding the raw turn", "error", err)
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

	seen := make(map[string]bool, r.TopK)
	out := make([]rag.Hit, 0, r.TopK)
	for _, h := range append(floor, rest...) {
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

// assemblePrompt numbers the surviving chunks. Numbering is positional, so the
// list handed to the model and the citations it is told to use cannot drift.
func assemblePrompt(hits []rag.Hit) string {
	var b strings.Builder
	b.WriteString("Sources:\n")
	for i, h := range hits {
		fmt.Fprintf(&b, "  [%d] (%s) %s\n", i+1, citation(h.Payload), collapse(h.Payload.Text))
	}
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
