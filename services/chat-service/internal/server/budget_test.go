package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	chatpb "chat-service/chat"
	"chat-service/internal/responder"
)

// blockingResponder holds the stream open until its context ends.
type blockingResponder struct{ started chan struct{} }

func (r *blockingResponder) Stream(ctx context.Context, _ *chatpb.ChatRequest, _ func(string) error) (responder.Usage, error) {
	close(r.started)
	<-ctx.Done()
	return responder.Usage{}, ctx.Err()
}

func errorInfo(t *testing.T, err error) *errdetails.ErrorInfo {
	t.Helper()
	st := status.Convert(err)
	if st.Code() != codes.ResourceExhausted {
		t.Fatalf("code = %v, want ResourceExhausted", st.Code())
	}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info
		}
	}
	t.Fatal("no ErrorInfo detail")
	return nil
}

func TestChatChargesTheTurnAndReportsTheBalance(t *testing.T) {
	srv := newTestServer()
	srv.responder = &usageResponder{usage: responder.Usage{InputTokens: 300, OutputTokens: 100}}
	stream := newFakeStream(context.Background())

	if err := srv.Chat(&chatpb.ChatRequest{Messages: userHistory("hi")}, stream); err != nil {
		t.Fatal(err)
	}
	b := stream.terminal().GetBalance()
	if b.GetTokensUsed() != 400 || b.GetTokensRemaining() != 600 {
		t.Errorf("balance = %+v, want 400 used, 600 remaining", b)
	}
	if _, err := time.Parse(time.RFC3339, b.GetResetsAt()); err != nil {
		t.Errorf("resets_at %q is not RFC 3339: %v", b.GetResetsAt(), err)
	}
}

func TestChatRefusesASpentVisitor(t *testing.T) {
	srv := newTestServer()
	srv.ledger.Debit(testClientIP, 1000)
	stream := newFakeStream(context.Background())

	err := srv.Chat(&chatpb.ChatRequest{Messages: userHistory("hi")}, stream)
	info := errorInfo(t, err)
	if info.GetReason() != "BUDGET_EXHAUSTED" || info.GetDomain() != "chat.v1" {
		t.Errorf("ErrorInfo = %+v, want BUDGET_EXHAUSTED in chat.v1", info)
	}
	if _, perr := time.Parse(time.RFC3339, info.GetMetadata()["resets_at"]); perr != nil {
		t.Errorf("resets_at metadata %q is not RFC 3339", info.GetMetadata()["resets_at"])
	}
	if len(stream.sent) != 0 {
		t.Error("a refused turn sent frames")
	}
}

// A failing provider still billed what it emitted; skipping the debit makes
// failures free.
func TestChatDebitsPartialUsageOnError(t *testing.T) {
	srv := newTestServer()
	srv.responder = &usageResponder{
		usage: responder.Usage{InputTokens: 500, OutputTokens: 50},
		err:   errors.New("provider died"),
	}
	_ = srv.Chat(&chatpb.ChatRequest{Messages: userHistory("hi")}, newFakeStream(context.Background()))

	if b := srv.ledger.Balance(testClientIP); b.Used != 550 {
		t.Errorf("used = %d after a failed turn, want 550", b.Used)
	}
}

// Without Envoy's header every request would share one bucket that a single
// visitor could exhaust for everyone.
func TestChatRefusesWithoutClientAddress(t *testing.T) {
	srv := newTestServer()
	err := srv.Chat(&chatpb.ChatRequest{Messages: userHistory("hi")}, &fakeStream{ctx: context.Background()})
	if status.Code(err) != codes.Internal {
		t.Errorf("code = %v, want Internal", status.Code(err))
	}
}

// A closed tab surfaces as a cancelled stream. If its slot is not released,
// two closed tabs lock the visitor out until a restart.
func TestChatReleasesTheSlotWhenTheClientHangsUp(t *testing.T) {
	srv := newTestServer()
	for i := 0; i < 3; i++ {
		started := make(chan struct{})
		srv.responder = &blockingResponder{started: started}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- srv.Chat(&chatpb.ChatRequest{Messages: userHistory("hi")}, newFakeStream(ctx)) }()
		<-started
		cancel()
		<-done
	}
	srv.responder = &responder.EchoResponder{}
	if err := srv.Chat(&chatpb.ChatRequest{Messages: userHistory("hi")}, newFakeStream(context.Background())); err != nil {
		t.Errorf("visitor locked out after hang-ups: %v", err)
	}
}

// A turn stuck on a stalled provider or a bug of ours must end and free its
// slot; otherwise each one permanently shrinks the global stream cap.
func TestChatTurnDeadlineFreesTheSlot(t *testing.T) {
	srv := newTestServer()
	srv.turnTimeout = 20 * time.Millisecond
	before := counterValue(chatStreamsTotal.WithLabelValues("timeout"))
	// One more than MAX_STREAMS_PER_VISITOR: a leaked slot refuses the last.
	for i := 0; i < 3; i++ {
		srv.responder = &blockingResponder{started: make(chan struct{})}
		err := srv.Chat(&chatpb.ChatRequest{Messages: userHistory("hi")}, newFakeStream(context.Background()))
		if status.Code(err) != codes.DeadlineExceeded {
			t.Fatalf("turn %d: err = %v, want DeadlineExceeded", i, err)
		}
	}
	if got := counterValue(chatStreamsTotal.WithLabelValues("timeout")) - before; got != 3 {
		t.Errorf("timeout += %v, want 3", got)
	}
	srv.responder = &responder.EchoResponder{}
	if err := srv.Chat(&chatpb.ChatRequest{Messages: userHistory("hi")}, newFakeStream(context.Background())); err != nil {
		t.Errorf("visitor locked out after stuck turns: %v", err)
	}
}

func TestChatRefusesAThirdConcurrentStream(t *testing.T) {
	srv := newTestServer()
	r1, _ := srv.ledger.Acquire(testClientIP)
	r2, _ := srv.ledger.Acquire(testClientIP)
	defer r1()
	defer r2()

	before := counterValue(chatStreamsTotal.WithLabelValues("rejected_concurrency"))
	err := srv.Chat(&chatpb.ChatRequest{Messages: userHistory("hi")}, newFakeStream(context.Background()))
	if info := errorInfo(t, err); info.GetReason() != "TOO_MANY_STREAMS" {
		t.Errorf("reason = %q, want TOO_MANY_STREAMS", info.GetReason())
	}
	if _, ok := errorInfo(t, err).GetMetadata()["resets_at"]; ok {
		t.Error("TOO_MANY_STREAMS carries resets_at; a slot frees when a stream ends")
	}
	if got := counterValue(chatStreamsTotal.WithLabelValues("rejected_concurrency")) - before; got != 1 {
		t.Errorf("rejected_concurrency += %v, want exactly 1", got)
	}
}

func TestWarmupReportsTheBalance(t *testing.T) {
	srv := newTestServer()
	srv.ledger.Debit(testClientIP, 250)
	ctx := newFakeStream(context.Background()).Context()

	resp, err := srv.Warmup(ctx, &chatpb.WarmupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetBalance().GetTokensRemaining() != 750 {
		t.Errorf("balance = %+v, want 750 remaining", resp.GetBalance())
	}
}

// A miss here hides a k3s/Docker NAT that pools every visitor under one key.
func TestIsSharedAddress(t *testing.T) {
	for addr, want := range map[string]bool{
		"172.18.0.1":      true,
		"10.42.0.7":       true,
		"192.168.1.10":    true,
		"127.0.0.1":       true,
		"::ffff:10.0.0.1": true,
		"fd00::1":         true,
		"fe80::1":         true,
		"203.0.113.9":     false,
		"2001:db8:1:2::3": false,
		"not-an-address":  false,
	} {
		if got := isSharedAddress(addr); got != want {
			t.Errorf("isSharedAddress(%q) = %v, want %v", addr, got, want)
		}
	}
}
