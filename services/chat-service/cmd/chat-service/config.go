package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"

	"chat-service/internal/responder"
)

const defaultEchoDelay = 60 * time.Millisecond

// Provider and retrieval defaults. RETRIEVAL_MIN_SCORE is a guess tuned from
// chat_retrieval_top_score in Grafana, not argued about here.
const (
	defaultLLMBaseURL      = "http://ollama:11434/v1"
	defaultLLMModel        = "llama3.2:3b"
	defaultEmbedBaseURL    = "http://ollama:11434/v1"
	defaultEmbedModel      = "nomic-embed-text"
	defaultQdrantURL       = "http://qdrant:6333"
	defaultTopK            = 6
	defaultMinScore        = 0.5
	defaultBackgroundFloor = 2
)

// loadConfig parses the environment into a responder.Config. Every check that
// names an environment variable lives here, so a misconfigured deployment
// fails at startup against the variable the operator has to fix.
func loadConfig() (responder.Config, error) {
	kind := envOr("RESPONDER", "echo")
	if kind != "echo" && kind != "llm" {
		return responder.Config{}, fmt.Errorf("unknown RESPONDER %q, want echo or llm", kind)
	}
	cfg := responder.Config{Kind: kind, EchoDelay: echoDelay()}
	if kind == "echo" {
		return cfg, nil
	}

	if name := envOr("EMBEDDER", "ollama"); name != "ollama" && name != "openai" {
		return responder.Config{}, fmt.Errorf("unknown EMBEDDER %q, want ollama or openai", name)
	}

	cfg.BaseURL = envOr("LLM_BASE_URL", defaultLLMBaseURL)
	if err := validateBaseURL(cfg.BaseURL); err != nil {
		return responder.Config{}, err
	}
	cfg.Model = envOr("LLM_MODEL", defaultLLMModel)
	cfg.APIKey = os.Getenv("LLM_API_KEY")

	cfg.EmbedBaseURL = envOr("EMBED_BASE_URL", defaultEmbedBaseURL)
	if err := validateEmbedURL(cfg.EmbedBaseURL); err != nil {
		return responder.Config{}, err
	}
	cfg.EmbedModel = envOr("EMBED_MODEL", defaultEmbedModel)
	cfg.EmbedAPIKey = os.Getenv("EMBED_API_KEY")

	cfg.QdrantURL = envOr("QDRANT_URL", defaultQdrantURL)
	if err := validateQdrantURL(cfg.QdrantURL); err != nil {
		return responder.Config{}, err
	}

	// Checked before any network call. Both parse cleanly and would otherwise
	// fail silently on every turn.
	cfg.TopK = envInt("RETRIEVAL_TOP_K", defaultTopK)
	cfg.Floor = envInt("RETRIEVAL_BACKGROUND_FLOOR", defaultBackgroundFloor)
	cfg.MinScore = envFloat("RETRIEVAL_MIN_SCORE", defaultMinScore)
	if cfg.Floor >= cfg.TopK {
		return responder.Config{}, fmt.Errorf("RETRIEVAL_BACKGROUND_FLOOR=%d must be below RETRIEVAL_TOP_K=%d, or no slot is left for source and docs", cfg.Floor, cfg.TopK)
	}
	if cfg.MinScore < -1 || cfg.MinScore > 1 {
		return responder.Config{}, fmt.Errorf("RETRIEVAL_MIN_SCORE=%v is outside cosine similarity's range of -1 to 1", cfg.MinScore)
	}
	return cfg, nil
}

// echoDelay reads ECHO_DELAY_MS. Set it to 0 for load tests without
// artificial latency.
func echoDelay() time.Duration {
	if n, err := strconv.Atoi(os.Getenv("ECHO_DELAY_MS")); err == nil && n >= 0 {
		return time.Duration(n) * time.Millisecond
	}
	return defaultEchoDelay
}

func validateBaseURL(raw string) error   { return validateURLVar("LLM_BASE_URL", raw) }
func validateEmbedURL(raw string) error  { return validateURLVar("EMBED_BASE_URL", raw) }
func validateQdrantURL(raw string) error { return validateURLVar("QDRANT_URL", raw) }

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
// line in the environment file must not become top_k=0, which retrieves
// nothing and looks like an empty corpus.
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

// envOr treats an empty variable as unset, so a blank line falls back to the
// default instead of producing an empty model name.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
