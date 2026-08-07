package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	chatpb "chat-service/chat"
)

type chatServer struct {
	chatpb.UnimplementedChatServiceServer
	responder Responder
}

func (s *chatServer) Chat(req *chatpb.ChatRequest, stream grpc.ServerStreamingServer[chatpb.ChatChunk]) error {
	ctx := stream.Context()
	start := time.Now()

	if err := validateHistory(req.GetMessages()); err != nil {
		chatStreamsTotal.WithLabelValues("error").Inc()
		chatStreamDuration.Observe(time.Since(start).Seconds())
		return err
	}

	trace.SpanFromContext(ctx).SetAttributes(
		attribute.Int("chat.history_len", len(req.GetMessages())),
	)
	log := logWithTrace(ctx, slog.Default())
	log.Info("chat stream started", "history_len", len(req.GetMessages()))

	usage, err := s.responder.Stream(ctx, req.GetMessages(), func(delta string) error {
		if err := stream.Send(&chatpb.ChatChunk{
			Event: &chatpb.ChatChunk_TextDelta{TextDelta: delta},
		}); err != nil {
			return err
		}
		chatChunksSentTotal.Inc()
		return nil
	})
	if err != nil {
		// A cancelled context or a Canceled status from a hung-up client is
		// expected, not a fault; anything else is a genuine responder error.
		outcome := classifyOutcome(err)
		chatStreamsTotal.WithLabelValues(outcome).Inc()
		chatStreamDuration.Observe(time.Since(start).Seconds())
		if outcome == "cancelled" {
			log.Info("chat stream ended early", "error", err, "outcome", outcome)
		} else {
			log.Warn("chat stream ended early", "error", err, "outcome", outcome)
		}
		return err
	}

	if err := stream.Send(&chatpb.ChatChunk{
		Event: &chatpb.ChatChunk_Done{Done: &chatpb.Done{
			StopReason:   usage.StopReason,
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
		}},
	}); err != nil {
		chatStreamsTotal.WithLabelValues(classifyOutcome(err)).Inc()
		chatStreamDuration.Observe(time.Since(start).Seconds())
		return err
	}

	chatStreamsTotal.WithLabelValues("ok").Inc()
	chatStreamDuration.Observe(time.Since(start).Seconds())
	log.Info("chat stream completed", "output_tokens", usage.OutputTokens)
	return nil
}

// classifyOutcome maps a responder or send error to a terminal status for
// metrics and logging. A client disconnect can surface either as a wrapped
// context error (from the responder noticing ctx.Done) or as a gRPC status
// error with code Canceled (returned by stream.Send once the client has hung
// up) — errors.Is alone would miss the latter. Both are expected, not a
// fault; anything else is a genuine error, even if the context also happens
// to be cancelled at the same moment.
func classifyOutcome(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.Canceled {
		return "cancelled"
	}
	return "error"
}

// validateHistory rejects requests before any frame is sent, so the client
// sees a clean status rather than a half-written assistant message.
func validateHistory(msgs []*chatpb.Message) error {
	if len(msgs) == 0 {
		return status.Error(codes.InvalidArgument, "messages must not be empty")
	}
	last := msgs[len(msgs)-1]
	if last.GetRole() != chatpb.Role_ROLE_USER {
		return status.Error(codes.InvalidArgument, "last message must have role ROLE_USER")
	}
	if strings.TrimSpace(last.GetContent()) == "" {
		return status.Error(codes.InvalidArgument, "last message content must not be blank")
	}
	return nil
}
