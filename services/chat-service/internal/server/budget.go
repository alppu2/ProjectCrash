package server

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	chatpb "chat-service/chat"
	"chat-service/internal/budget"
	"chat-service/internal/obs"
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
	if isSharedAddress(vals[0]) {
		warnSharedAddressOnce.Do(func() {
			obs.LogWithTrace(ctx, slog.Default()).Warn("client address is private: a NAT or proxy in front of Envoy pools visitors under one budget key", "client_ip", vals[0])
		})
	}
	return budget.VisitorKey(vals[0])
}

// Once per process: a misconfigured deployment would otherwise log every turn.
var warnSharedAddressOnce sync.Once

// isSharedAddress reports an address no public visitor arrives from, meaning
// the edge saw a NAT or proxy instead of the visitor.
func isSharedAddress(addr string) bool {
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return false
	}
	a = a.Unmap()
	return a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast()
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
