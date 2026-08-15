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

// echoDelay reads ECHO_DELAY_MS, falling back to defaultEchoDelay. Lowering it
// to 0 makes load tests run without artificial latency.
func echoDelay() time.Duration {
	if n, err := strconv.Atoi(os.Getenv("ECHO_DELAY_MS")); err == nil && n >= 0 {
		return time.Duration(n) * time.Millisecond
	}
	return defaultEchoDelay
}

// newResponder builds the Responder named by RESPONDER, defaulting to the echo
// stub so ghz load tests and CI run with no model and no GPU. An unrecognised
// name is a startup error rather than a silent fallback: a demo that quietly
// answers with echo looks like a working model.
func newResponder() (Responder, error) {
	switch name := envOr("RESPONDER", "echo"); name {
	case "echo":
		slog.Info("responder configured", "responder", "echo")
		return &EchoResponder{Delay: echoDelay()}, nil

	case "llm":
		baseURL := envOr("LLM_BASE_URL", defaultLLMBaseURL)
		if _, err := url.Parse(baseURL); err != nil {
			return nil, fmt.Errorf("LLM_BASE_URL %q is not a valid URL: %w", baseURL, err)
		}
		model := envOr("LLM_MODEL", defaultLLMModel)
		apiKey := os.Getenv("LLM_API_KEY")
		// The key itself is never logged; whether one is set is worth knowing
		// when a provider starts returning 401.
		slog.Info("responder configured", "responder", "llm",
			"base_url", baseURL, "model", model, "api_key_set", apiKey != "")
		return &OpenAIResponder{
			// Trimmed because the request path is appended directly.
			BaseURL: strings.TrimSuffix(baseURL, "/"),
			Model:   model,
			APIKey:  apiKey,
			Client:  newLLMClient(),
		}, nil

	default:
		return nil, fmt.Errorf("unknown RESPONDER %q, want echo or llm", name)
	}
}

// envOr treats an empty variable as unset, so a commented-out or blank line in
// .env falls back to the default instead of producing an empty model name.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
