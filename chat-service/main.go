package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"

	chatpb "chat-service/chat"
	"chat-service/internal/rag"
)

const defaultEchoDelay = 60 * time.Millisecond

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	go func() {
		http.Handle("/metrics", promhttp.Handler())
		if err := http.ListenAndServe(":9091", nil); err != nil {
			slog.Error("metrics server failed", "error", err)
			os.Exit(1)
		}
	}()

	otelCtx, otelCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer otelCancel()
	shutdown, err := initTracer(otelCtx)
	if err != nil {
		slog.Warn("failed to init tracer, continuing without tracing", "error", err)
	} else {
		defer shutdown()
	}

	grpcPort := os.Getenv("GRPC_PORT")
	if grpcPort == "" {
		grpcPort = ":50051"
	}
	lis, err := net.Listen("tcp", grpcPort)
	if err != nil {
		slog.Error("failed to listen", "error", err, "port", grpcPort)
		os.Exit(1)
	}

	grpcServer := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
	)
	responder, err := newResponder(context.Background())
	if err != nil {
		slog.Error("failed to build responder", "error", err)
		os.Exit(1)
	}
	chatpb.RegisterChatServiceServer(grpcServer, &chatServer{responder: responder})

	slog.Info("chat service listening", "port", grpcPort)
	if err := grpcServer.Serve(lis); err != nil {
		slog.Error("grpc server failed", "error", err)
		os.Exit(1)
	}
}

// echoDelay reads ECHO_DELAY_MS. Set it to 0 for load tests without
// artificial latency.
func echoDelay() time.Duration {
	if n, err := strconv.Atoi(os.Getenv("ECHO_DELAY_MS")); err == nil && n >= 0 {
		return time.Duration(n) * time.Millisecond
	}
	return defaultEchoDelay
}

// newResponder builds the Responder named by RESPONDER, defaulting to the echo
// stub so load tests and CI run with no model and no GPU. An unknown name is a
// startup error, not a silent fallback: a demo quietly answering with echo
// looks like a working model.
func newResponder(ctx context.Context) (Responder, error) {
	switch name := envOr("RESPONDER", "echo"); name {
	case "echo":
		slog.Info("responder configured", "responder", "echo")
		return &EchoResponder{Delay: echoDelay()}, nil

	case "llm":
		baseURL := envOr("LLM_BASE_URL", defaultLLMBaseURL)
		if err := validateBaseURL(baseURL); err != nil {
			return nil, err
		}
		model := envOr("LLM_MODEL", defaultLLMModel)
		apiKey := os.Getenv("LLM_API_KEY")

		inner := &OpenAIResponder{
			// The request path is appended directly.
			BaseURL: strings.TrimSuffix(baseURL, "/"),
			Model:   model,
			APIKey:  apiKey,
			Client:  newLLMClient(),
		}

		retriever, err := newRetriever(ctx, inner)
		if err != nil {
			return nil, err
		}
		// Logs whether a key is set, never the key. Worth knowing on a 401.
		slog.Info("responder configured", "responder", "llm",
			"base_url", baseURL, "model", model, "api_key_set", apiKey != "",
			"embed_model", retriever.Embedder.ModelID(), "top_k", retriever.TopK)
		return retriever, nil

	default:
		return nil, fmt.Errorf("unknown RESPONDER %q, want echo or llm", name)
	}
}

// newRetriever builds the retrieval side. Every assertion here is a startup
// error: with nothing stuffed into the prompt there is no grounding to fall
// back to, and serving ungrounded answers about a real person is worse than
// not serving.
func newRetriever(ctx context.Context, inner *OpenAIResponder) (*RetrievingResponder, error) {
	if name := envOr("EMBEDDER", "ollama"); name != "ollama" && name != "openai" {
		return nil, fmt.Errorf("unknown EMBEDDER %q, want ollama or openai", name)
	}

	embedBase := envOr("EMBED_BASE_URL", defaultEmbedBaseURL)
	if err := validateEmbedURL(embedBase); err != nil {
		return nil, err
	}
	embedder, err := rag.NewOpenAIEmbedder(ctx, embedBase,
		envOr("EMBED_MODEL", defaultEmbedModel), os.Getenv("EMBED_API_KEY"), newLLMClient())
	if err != nil {
		return nil, err
	}

	store := &rag.Store{
		BaseURL:    envOr("QDRANT_URL", defaultQdrantURL),
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
		TopK:      envInt("RETRIEVAL_TOP_K", defaultTopK),
		MinScore:  float32(envFloat("RETRIEVAL_MIN_SCORE", defaultMinScore)),
		Floor:     envInt("RETRIEVAL_BACKGROUND_FLOOR", defaultBackgroundFloor),
	}, nil
}

// Retrieval defaults. RETRIEVAL_MIN_SCORE is a guess tuned from
// chat_retrieval_top_score in Grafana, not argued about here.
const (
	defaultEmbedBaseURL    = "http://ollama:11434/v1"
	defaultEmbedModel      = "nomic-embed-text"
	defaultQdrantURL       = "http://qdrant:6333"
	defaultTopK            = 6
	defaultMinScore        = 0.5
	defaultBackgroundFloor = 2
)

func validateBaseURL(raw string) error  { return validateURLVar("LLM_BASE_URL", raw) }
func validateEmbedURL(raw string) error { return validateURLVar("EMBED_BASE_URL", raw) }

// validateURLVar rejects a URL that would boot cleanly and fail every turn.
// url.Parse alone accepts "ollama:11434/v1" (opaque) and "not a url" (bare
// path); both surface later as reason="unreachable", pointing an operator at a
// healthy provider. A missing scheme is the likely typo.
func validateURLVar(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s %q is not a valid URL: %w", name, raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s %q needs an http:// or https:// scheme", name, raw)
	}
	if u.Host == "" {
		return fmt.Errorf("%s %q has no host", name, raw)
	}
	// Userinfo would reach Loki via the base_url startup log and the browser
	// via an unreachable error. Names the host, never raw, which holds the
	// password. The matching *_API_KEY is the only supported place for a
	// credential.
	if u.User != nil {
		return fmt.Errorf("%s for host %q must not embed credentials; use the matching *_API_KEY", name, u.Host)
	}
	return nil
}

// envInt rejects a non-positive value as well as an unparseable one: a blank
// line in .env must not become top_k=0, which retrieves nothing and looks like
// an empty corpus.
func envInt(key string, fallback int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil && n > 0 {
		return n
	}
	return fallback
}

func envFloat(key string, fallback float64) float64 {
	if f, err := strconv.ParseFloat(os.Getenv(key), 64); err == nil {
		return f
	}
	return fallback
}

// envOr treats an empty variable as unset, so a blank line in .env falls back
// to the default instead of producing an empty model name.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
