package responder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"chat-service/internal/rag"
)

// Config is the environment a Responder is built from, already parsed and
// range-checked by the caller. Every error New returns is about state it had
// to reach out to discover — a missing collection, a dimension mismatch.
type Config struct {
	// Kind selects the implementation: "echo" or "llm".
	Kind      string
	EchoDelay time.Duration

	BaseURL string
	Model   string
	APIKey  string

	EmbedBaseURL string
	EmbedModel   string
	EmbedAPIKey  string

	QdrantURL string
	TopK      int
	MinScore  float64
	Floor     int
}

// New builds the Responder named by cfg.Kind. An unknown name is an error, not
// a silent fallback: a demo quietly answering with echo looks like a working
// model.
func New(ctx context.Context, cfg Config) (Responder, error) {
	switch cfg.Kind {
	case "echo":
		slog.Info("responder configured", "responder", "echo")
		return &EchoResponder{Delay: cfg.EchoDelay}, nil

	case "llm":
		inner := &OpenAIResponder{
			// The request path is appended directly.
			BaseURL: strings.TrimSuffix(cfg.BaseURL, "/"),
			Model:   cfg.Model,
			APIKey:  cfg.APIKey,
			Client:  newLLMClient(),
		}

		retriever, err := newRetriever(ctx, cfg, inner)
		if err != nil {
			return nil, err
		}
		// Logs whether a key is set, never the key. Worth knowing on a 401.
		slog.Info("responder configured", "responder", "llm",
			"base_url", cfg.BaseURL, "model", cfg.Model, "api_key_set", cfg.APIKey != "",
			"embed_model", retriever.Embedder.ModelID(), "top_k", retriever.TopK)
		return retriever, nil

	default:
		return nil, fmt.Errorf("unknown RESPONDER %q, want echo or llm", cfg.Kind)
	}
}

// newRetriever builds the retrieval side. Every assertion here is a startup
// error: with nothing stuffed into the prompt there is no grounding to fall
// back to, and serving ungrounded answers about a real person is worse than
// not serving.
func newRetriever(ctx context.Context, cfg Config, inner *OpenAIResponder) (*RetrievingResponder, error) {
	embedder, err := rag.NewOpenAIEmbedder(ctx, cfg.EmbedBaseURL, cfg.EmbedModel, cfg.EmbedAPIKey, newLLMClient())
	if err != nil {
		return nil, err
	}

	store := &rag.Store{
		BaseURL:    cfg.QdrantURL,
		Collection: rag.CollectionName(embedder),
		HTTP:       newLLMClient(),
	}

	dims, points, err := store.Info(ctx)
	if err != nil {
		if errors.Is(err, rag.ErrCollectionMissing) {
			return nil, fmt.Errorf("collection %s does not exist; build it with: docker compose run --rm ingest", store.Collection)
		}
		return nil, err
	}
	if dims != embedder.Dims() {
		return nil, fmt.Errorf("collection %s has %d dimensions, %s produces %d", store.Collection, dims, embedder.ModelID(), embedder.Dims())
	}
	// An empty collection presents as a chat that works but knows nothing.
	if points == 0 {
		return nil, fmt.Errorf("collection %s is empty; build it with: docker compose run --rm ingest", store.Collection)
	}

	return &RetrievingResponder{
		Inner:     inner,
		Embedder:  embedder,
		Store:     store,
		Condenser: inner,
		TopK:      cfg.TopK,
		MinScore:  float32(cfg.MinScore),
		Floor:     cfg.Floor,
	}, nil
}
