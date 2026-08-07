package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	chatpb "chat-service/chat"
)

// counterValue reads the current value of a prometheus counter without
// pulling in the testutil subpackage, which needs go.sum entries this repo
// hasn't resolved (kylelemons/godebug).
func counterValue(c prometheus.Counter) float64 {
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

// fakeStream satisfies grpc.ServerStreamingServer[chatpb.ChatChunk] by
// collecting sends in memory. The embedded nil interface supplies the methods
// the handler never calls; touching one would panic, which is the intent.
type fakeStream struct {
	grpc.ServerStream
	ctx     context.Context
	sent    []*chatpb.ChatChunk
	sendErr error
}

func newFakeStream(ctx context.Context) *fakeStream {
	return &fakeStream{ctx: ctx}
}

func (f *fakeStream) Context() context.Context { return f.ctx }

func (f *fakeStream) Send(c *chatpb.ChatChunk) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, c)
	return nil
}

// texts returns the text_delta payloads, in order.
func (f *fakeStream) texts() []string {
	var out []string
	for _, c := range f.sent {
		if c.GetTextDelta() != "" {
			out = append(out, c.GetTextDelta())
		}
	}
	return out
}

// terminal returns the Done frame, or nil if the stream never sent one.
func (f *fakeStream) terminal() *chatpb.Done {
	for _, c := range f.sent {
		if d := c.GetDone(); d != nil {
			return d
		}
	}
	return nil
}

func newTestServer() *chatServer {
	return &chatServer{responder: &EchoResponder{}}
}

// erroringResponder always returns a fixed error, ignoring ctx entirely. It
// lets tests produce a genuine responder failure independent of whether the
// stream's context happens to be cancelled — EchoResponder can't do that,
// since its only error path is ctx cancellation.
type erroringResponder struct {
	err error
}

func (r *erroringResponder) Stream(ctx context.Context, req *chatpb.ChatRequest, emit func(delta string) error) (Usage, error) {
	return Usage{}, r.err
}

func TestChatStreamsDeltasThenDone(t *testing.T) {
	srv := newTestServer()
	stream := newFakeStream(context.Background())
	req := &chatpb.ChatRequest{Messages: []*chatpb.Message{
		{Role: chatpb.Role_ROLE_USER, Content: "hello there"},
	}}

	if err := srv.Chat(req, stream); err != nil {
		t.Fatalf("Chat() error = %v, want nil", err)
	}

	if want := []string{"hello", " there"}; !slices.Equal(stream.texts(), want) {
		t.Errorf("deltas = %q, want %q", stream.texts(), want)
	}
	if got := strings.Join(stream.texts(), ""); got != "hello there" {
		t.Errorf("concatenated deltas = %q, want %q", got, "hello there")
	}

	done := stream.terminal()
	if done == nil {
		t.Fatal("no Done frame sent")
	}
	if done.GetStopReason() != "end_turn" {
		t.Errorf("StopReason = %q, want %q", done.GetStopReason(), "end_turn")
	}
	if done.GetOutputTokens() != 2 {
		t.Errorf("OutputTokens = %d, want 2", done.GetOutputTokens())
	}
	if last := stream.sent[len(stream.sent)-1]; last.GetDone() == nil {
		t.Error("Done frame is not last in the stream")
	}
}

func TestChatRejectsInvalidHistory(t *testing.T) {
	tests := []struct {
		name string
		msgs []*chatpb.Message
	}{
		{"empty history", nil},
		{
			"last message is assistant",
			[]*chatpb.Message{
				{Role: chatpb.Role_ROLE_USER, Content: "hi"},
				{Role: chatpb.Role_ROLE_ASSISTANT, Content: "hello"},
			},
		},
		{
			"last message has empty content",
			[]*chatpb.Message{{Role: chatpb.Role_ROLE_USER, Content: "   "}},
		},
		{
			"last message role unset",
			[]*chatpb.Message{{Role: chatpb.Role_ROLE_UNSPECIFIED, Content: "hi"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServer()
			stream := newFakeStream(context.Background())

			err := srv.Chat(&chatpb.ChatRequest{Messages: tt.msgs}, stream)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("Chat() code = %v, want InvalidArgument (err = %v)", status.Code(err), err)
			}
			if len(stream.sent) != 0 {
				t.Errorf("sent %d chunks, want 0 — validation must run before any send", len(stream.sent))
			}
		})
	}
}

// repeatMessages builds n user messages, each with the given content, used to
// probe the maxHistoryMessages and maxHistoryBytes bounds in validateHistory.
func repeatMessages(n int, content string) []*chatpb.Message {
	msgs := make([]*chatpb.Message, n)
	for i := range msgs {
		msgs[i] = &chatpb.Message{Role: chatpb.Role_ROLE_USER, Content: content}
	}
	return msgs
}

func TestValidateHistoryBounds(t *testing.T) {
	tests := []struct {
		name    string
		msgs    []*chatpb.Message
		wantErr bool
	}{
		{
			name:    "message count over limit is rejected",
			msgs:    repeatMessages(maxHistoryMessages+1, "hi"),
			wantErr: true,
		},
		{
			name:    "content bytes over limit is rejected",
			msgs:    []*chatpb.Message{{Role: chatpb.Role_ROLE_USER, Content: strings.Repeat("a", maxHistoryBytes+1)}},
			wantErr: true,
		},
		{
			name:    "message count just under limit is accepted",
			msgs:    repeatMessages(maxHistoryMessages, "hi"),
			wantErr: false,
		},
		{
			name:    "content bytes just under limit is accepted",
			msgs:    []*chatpb.Message{{Role: chatpb.Role_ROLE_USER, Content: strings.Repeat("a", maxHistoryBytes)}},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateHistory(tt.msgs)
			if tt.wantErr {
				if status.Code(err) != codes.InvalidArgument {
					t.Fatalf("validateHistory() code = %v, want InvalidArgument (err = %v)", status.Code(err), err)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateHistory() error = %v, want nil", err)
			}
		})
	}
}

func TestChatStopsOnClientCancel(t *testing.T) {
	srv := &chatServer{responder: &EchoResponder{Delay: 20 * time.Millisecond}}
	ctx, cancel := context.WithCancel(context.Background())
	stream := newFakeStream(ctx)
	req := &chatpb.ChatRequest{Messages: []*chatpb.Message{
		{Role: chatpb.Role_ROLE_USER, Content: "one two three four five"},
	}}

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := srv.Chat(req, stream)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Chat() error = %v, want context.Canceled", err)
	}
	if stream.terminal() != nil {
		t.Error("Done frame sent after cancel, want none")
	}
	if len(stream.texts()) >= 5 {
		t.Errorf("sent %d deltas, want fewer than 5 (stream should stop early)", len(stream.texts()))
	}
}

func TestChatDoesNotMisclassifyErrorAsCancelled(t *testing.T) {
	sentinel := errors.New("model overloaded")
	srv := &chatServer{responder: &erroringResponder{err: sentinel}}

	// Cancel the context before Stream even runs, so the responder's error
	// and the client's cancellation land at the same moment. classifyOutcome
	// must trust the error, not ctx, or a real fault gets hidden as a
	// routine disconnect.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stream := newFakeStream(ctx)
	req := &chatpb.ChatRequest{Messages: []*chatpb.Message{
		{Role: chatpb.Role_ROLE_USER, Content: "hello"},
	}}

	errBefore := counterValue(chatStreamsTotal.WithLabelValues("error"))
	cancelledBefore := counterValue(chatStreamsTotal.WithLabelValues("cancelled"))

	err := srv.Chat(req, stream)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Chat() error = %v, want %v", err, sentinel)
	}

	if got := counterValue(chatStreamsTotal.WithLabelValues("error")); got != errBefore+1 {
		t.Errorf(`chatStreamsTotal{status="error"} = %v, want %v`, got, errBefore+1)
	}
	if got := counterValue(chatStreamsTotal.WithLabelValues("cancelled")); got != cancelledBefore {
		t.Errorf(`chatStreamsTotal{status="cancelled"} = %v, want unchanged at %v (error was misclassified as cancelled)`, got, cancelledBefore)
	}
}

func TestChatPropagatesSendError(t *testing.T) {
	srv := newTestServer()
	stream := newFakeStream(context.Background())
	stream.sendErr = errors.New("transport closed")
	req := &chatpb.ChatRequest{Messages: []*chatpb.Message{
		{Role: chatpb.Role_ROLE_USER, Content: "hello"},
	}}

	if err := srv.Chat(req, stream); err == nil {
		t.Fatal("Chat() error = nil, want the transport error")
	}
}
