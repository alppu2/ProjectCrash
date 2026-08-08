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

// Chat records exactly one chatStreamsTotal increment and one
// chatStreamDuration observation per call, regardless of which exit path is
// taken. outcome starts at "ok" and is only ever downgraded, and the single
// deferred func is the one place that touches either metric — do not add
// another Inc/Observe pair anywhere else in this function.
func (s *chatServer) Chat(req *chatpb.ChatRequest, stream grpc.ServerStreamingServer[chatpb.ChatChunk]) error {
	ctx := stream.Context()
	start := time.Now()
	outcome := "ok"

	defer func() {
		chatStreamsTotal.WithLabelValues(outcome).Inc()
		chatStreamDuration.Observe(time.Since(start).Seconds())
	}()

	if err := validateHistory(req.GetMessages()); err != nil {
		outcome = "error"
		return err
	}

	trace.SpanFromContext(ctx).SetAttributes(
		attribute.Int("chat.history_len", len(req.GetMessages())),
	)
	log := logWithTrace(ctx, slog.Default())
	log.Info("chat stream started", "history_len", len(req.GetMessages()))

	usage, err := s.responder.Stream(ctx, req, func(delta string) error {
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
		// usage may be partially populated even though err is non-nil (see
		// the Usage doc comment) — a real provider that fails after emitting
		// 400 tokens still bills for them, so log what was produced rather
		// than discarding it. No Done frame is sent on this path.
		outcome = classifyOutcome(err)
		if outcome == "cancelled" {
			log.Info("chat stream ended early", "error", err, "outcome", outcome,
				"partial_input_tokens", usage.InputTokens, "partial_output_tokens", usage.OutputTokens)
		} else {
			log.Warn("chat stream ended early", "error", err, "outcome", outcome,
				"partial_input_tokens", usage.InputTokens, "partial_output_tokens", usage.OutputTokens)
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
		outcome = classifyOutcome(err)
		return err
	}

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

// maxHistoryMessages and maxHistoryBytes bound request size at the server,
// since the endpoint is unauthenticated and the client-side MAX_HISTORY cap
// is advisory only — a caller that skips the frontend can send anything.
const (
	maxHistoryMessages = 40
	maxHistoryBytes    = 32768
)

// validateHistory rejects requests before any frame is sent, so the client
// sees a clean status rather than a half-written assistant message.
func validateHistory(msgs []*chatpb.Message) error {
	if len(msgs) == 0 {
		return status.Error(codes.InvalidArgument, "messages must not be empty")
	}
	if len(msgs) > maxHistoryMessages {
		return status.Errorf(codes.InvalidArgument, "history has %d messages, which exceeds the limit of %d", len(msgs), maxHistoryMessages)
	}
	var totalBytes int
	for _, m := range msgs {
		totalBytes += len(m.GetContent())
	}
	if totalBytes > maxHistoryBytes {
		return status.Errorf(codes.InvalidArgument, "history content is %d bytes, which exceeds the limit of %d bytes", totalBytes, maxHistoryBytes)
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
