package main

import (
	"context"
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
	responder, err := newResponder()
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
func newResponder() (Responder, error) {
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
		// Logs whether a key is set, never the key. Worth knowing on a 401.
		slog.Info("responder configured", "responder", "llm",
			"base_url", baseURL, "model", model, "api_key_set", apiKey != "")
		return &OpenAIResponder{
			// The request path is appended directly.
			BaseURL: strings.TrimSuffix(baseURL, "/"),
			Model:   model,
			APIKey:  apiKey,
			Client:  newLLMClient(),
		}, nil

	default:
		return nil, fmt.Errorf("unknown RESPONDER %q, want echo or llm", name)
	}
}

// validateBaseURL rejects a URL that would boot cleanly and fail every turn.
// url.Parse alone accepts "ollama:11434/v1" (opaque) and "not a url" (bare
// path); both surface later as reason="unreachable", pointing an operator at a
// healthy provider. A missing scheme is the likely typo.
func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("LLM_BASE_URL %q is not a valid URL: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("LLM_BASE_URL %q needs an http:// or https:// scheme", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("LLM_BASE_URL %q has no host", raw)
	}
	// Userinfo would reach Loki via the base_url startup log and the browser
	// via an unreachable error. Names the host, never raw, which holds the
	// password. LLM_API_KEY is the only supported place for a credential.
	if u.User != nil {
		return fmt.Errorf("LLM_BASE_URL for host %q must not embed credentials; use LLM_API_KEY", u.Host)
	}
	return nil
}

// envOr treats an empty variable as unset, so a blank line in .env falls back
// to the default instead of producing an empty model name.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
