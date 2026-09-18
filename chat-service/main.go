package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"

	chatpb "chat-service/chat"
	"chat-service/internal/responder"
)

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
	resp, err := newResponder(context.Background())
	if err != nil {
		slog.Error("failed to build responder", "error", err)
		os.Exit(1)
	}
	chatpb.RegisterChatServiceServer(grpcServer, &chatServer{responder: resp})

	slog.Info("chat service listening", "port", grpcPort)
	if err := grpcServer.Serve(lis); err != nil {
		slog.Error("grpc server failed", "error", err)
		os.Exit(1)
	}
}

// newResponder is the seam the startup tests drive: environment in, a wired
// Responder out. Both halves fail loudly, so a bad deployment never serves.
func newResponder(ctx context.Context) (responder.Responder, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	return responder.New(ctx, cfg)
}
