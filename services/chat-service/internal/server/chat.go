package server

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
	"chat-service/internal/budget"
	"chat-service/internal/history"
	"chat-service/internal/obs"
	"chat-service/internal/responder"
)

type chatServer struct {
	chatpb.UnimplementedChatServiceServer
	responder responder.Responder
	warmer    responder.Warmer // nil: nothing to warm, always ready
	signer    *history.Signer
	ledger    *budget.Ledger
	// turnTimeout bounds one turn, so a stuck one cannot hold its stream slot.
	turnTimeout time.Duration
}

// New returns the ChatService handler for r, signing replies with signer and
// charging each turn to ledger.
func New(r responder.Responder, signer *history.Signer, ledger *budget.Ledger, turnTimeout time.Duration) chatpb.ChatServiceServer {
	// Echo has no Warmer, so the assertion leaves warmer nil.
	warmer, _ := r.(responder.Warmer)
	return &chatServer{responder: r, warmer: warmer, signer: signer, ledger: ledger, turnTimeout: turnTimeout}
}

// Chat records exactly one chatStreamsTotal increment and one
// chatStreamDuration observation per call, from the single deferred func.
// outcome is only ever downgraded; do not add another Inc/Observe pair.
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

	key, err := visitorKey(ctx)
	if err != nil {
		outcome = "error"
		obs.LogWithTrace(ctx, slog.Default()).Error("refusing a turn without a usable client address; is Envoy setting x-client-ip?", "error", err)
		return status.Error(codes.Internal, "the assistant is misconfigured")
	}
	release, err := s.ledger.Acquire(key)
	if err != nil {
		var refused *budget.RefusedError
		if errors.As(err, &refused) {
			outcome = refusalOutcome(refused)
			return refusalStatus(refused)
		}
		outcome = "error"
		return status.Error(codes.Internal, "budget check failed")
	}
	defer release()

	// Bounds how long a slot can be held; release above frees it either way.
	turnCtx, cancelTurn := context.WithTimeout(ctx, s.turnTimeout)
	defer cancelTurn()

	msgs, dropped := s.signer.Verify(req.GetMessages())
	for reason, n := range dropped {
		chatHistoryDroppedTotal.WithLabelValues(reason).Add(float64(n))
	}
	req = &chatpb.ChatRequest{Messages: msgs}
	question := msgs[len(msgs)-1].GetContent()

	trace.SpanFromContext(ctx).SetAttributes(
		attribute.Int("chat.history_len", len(req.GetMessages())),
	)
	log := obs.LogWithTrace(ctx, slog.Default())
	log.Info("chat stream started", "history_len", len(req.GetMessages()))

	var reply strings.Builder
	usage, err := s.responder.Stream(turnCtx, req, func(delta string) error {
		if err := stream.Send(&chatpb.ChatChunk{
			Event: &chatpb.ChatChunk_TextDelta{TextDelta: delta},
		}); err != nil {
			return err
		}
		// Only what reached the client is signed: that is what it sends back.
		reply.WriteString(delta)
		chatChunksSentTotal.Inc()
		return nil
	})

	// Before branching on err: a mid-stream failure still consumed the tokens
	// it reports. Separate metric from the single-site rule above.
	recordTokens(usage)
	// Charged on every path: a failed turn still consumed what it reports.
	balance := s.ledger.Debit(key, int64(usage.InputTokens)+int64(usage.OutputTokens))

	if err != nil {
		// Our deadline, not the client's: the stream context is still live.
		if errors.Is(turnCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			outcome = "timeout"
			log.Warn("chat turn hit its deadline", "timeout", s.turnTimeout,
				"partial_input_tokens", usage.InputTokens, "partial_output_tokens", usage.OutputTokens)
			return status.Error(codes.DeadlineExceeded, "the reply took too long")
		}
		// No Done frame on this path. usage may still be partial — log it.
		outcome = responder.ClassifyOutcome(err)
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
			Signature:    s.signer.Sign(question, reply.String()),
			Balance:      toProtoBalance(balance),
		}},
	}); err != nil {
		outcome = responder.ClassifyOutcome(err)
		return err
	}

	log.Info("chat stream completed", "output_tokens", usage.OutputTokens)
	return nil
}

// Warmup records exactly one chatWarmupsTotal increment and one
// chatWarmupDuration observation, from the single deferred func.
func (s *chatServer) Warmup(ctx context.Context, _ *chatpb.WarmupRequest) (*chatpb.WarmupResponse, error) {
	start := time.Now()
	outcome := "ok"
	defer func() {
		chatWarmupsTotal.WithLabelValues(outcome).Inc()
		chatWarmupDuration.Observe(time.Since(start).Seconds())
	}()

	resp := &chatpb.WarmupResponse{}
	if key, err := visitorKey(ctx); err == nil {
		resp.Balance = toProtoBalance(s.ledger.Balance(key))
	}

	if s.warmer == nil {
		return resp, nil
	}
	if err := s.warmer.Warmup(ctx); err != nil {
		outcome = responder.ClassifyOutcome(err)
		log := obs.LogWithTrace(ctx, slog.Default())
		if outcome == "cancelled" {
			log.Info("warmup abandoned", "error", err)
			return nil, status.Error(codes.Canceled, "warmup cancelled")
		}
		// Provider error text can carry account detail; it stays in the log.
		log.Warn("warmup failed", "error", err)
		return nil, status.Error(codes.Unavailable, "the assistant is unavailable")
	}
	return resp, nil
}

// The endpoint is unauthenticated, so the client-side MAX_HISTORY cap is
// advisory only — a caller that skips the frontend can send anything.
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
	// The wire format has no equivalent of an unset role, so a Responder would
	// have to guess at ROLE_UNSPECIFIED. Reject it here instead.
	for i, m := range msgs {
		if m.GetRole() == chatpb.Role_ROLE_UNSPECIFIED {
			return status.Errorf(codes.InvalidArgument, "message %d has no role; every message must be ROLE_USER or ROLE_ASSISTANT", i)
		}
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
