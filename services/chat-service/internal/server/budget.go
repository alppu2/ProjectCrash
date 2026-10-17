package server

import (
	"context"
	"errors"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	chatpb "chat-service/chat"
	"chat-service/internal/budget"
)

// clientIPHeader is written by Envoy, overwriting any client-sent value.
// chat-service has no published port, so nothing else can set it.
const clientIPHeader = "x-client-ip"

var errNoClientAddress = errors.New("no client address from the proxy")

func visitorKey(ctx context.Context) (string, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get(clientIPHeader)
	if len(vals) != 1 {
		return "", errNoClientAddress
	}
	return budget.VisitorKey(vals[0])
}

// refusalStatus carries the reason as ErrorInfo so the frontend switches on a
// field, never on message text.
func refusalStatus(r *budget.RefusedError) error {
	md := map[string]string{}
	if !r.ResetsAt.IsZero() {
		md["resets_at"] = r.ResetsAt.UTC().Format(time.RFC3339)
	}
	st, err := status.New(codes.ResourceExhausted, "turn refused: "+string(r.Reason)).
		WithDetails(&errdetails.ErrorInfo{Reason: string(r.Reason), Domain: "chat.v1", Metadata: md})
	if err != nil {
		return status.Error(codes.ResourceExhausted, "turn refused: "+string(r.Reason))
	}
	return st.Err()
}

func refusalOutcome(r *budget.RefusedError) string {
	if r.Reason == budget.TooManyStreams {
		return "rejected_concurrency"
	}
	return "rejected_budget"
}

func toProtoBalance(b budget.Balance) *chatpb.Balance {
	return &chatpb.Balance{
		TokensUsed:      int32(b.Used),
		TokensRemaining: int32(b.Remaining),
		ResetsAt:        b.ResetsAt.UTC().Format(time.RFC3339),
	}
}
