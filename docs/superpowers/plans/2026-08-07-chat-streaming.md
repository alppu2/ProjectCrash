# Streaming Chat Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a chat panel to the frontend backed by a new `chat-service` that streams replies token-by-token over a gRPC server-streaming RPC, using an echo stub in place of an LLM.

**Architecture:** A new `chat-service` container exposes `rpc Chat(ChatRequest) returns (stream ChatChunk)`. Envoy routes `/chat.v1.ChatService/` to it by path prefix while everything else keeps going to `order-service`. The reply generator sits behind a `Responder` interface, so roadmap step 3 swaps `EchoResponder` for a Claude-backed one without touching the handler, the proto, or the frontend. Conversation history lives on the client and is sent whole on every turn, so the service holds no per-user state.

**Tech Stack:** Go 1.26, grpc-go, protoc-gen-go / protoc-gen-go-grpc, Prometheus client_golang, OpenTelemetry (otelgrpc + OTLP to Tempo), Envoy grpc-web, React 19 + Vite, connect-web, buf.

**Spec:** `docs/superpowers/specs/2026-08-07-chat-streaming-design.md`

## Global Constraints

- Go module directive: `go 1.26.3` (matches `order-service` and `inventory-service`).
- Dependency versions must match the rest of the repo exactly: `google.golang.org/grpc v1.81.1`, `google.golang.org/protobuf v1.36.11`, `github.com/prometheus/client_golang v1.23.2`, `go.opentelemetry.io/otel v1.44.0` (and the `sdk` / `otlptracegrpc` modules at the same version), `go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.69.0`.
- Proto package is `chat.v1`. This determines the gRPC URL path `/chat.v1.ChatService/Chat`, which the Envoy route matches literally. Changing the package means changing the Envoy route.
- gRPC listens on `GRPC_PORT` (default `:50051`), Prometheus metrics on a hardcoded `:9091`. Same convention as the other two services.
- Logs are JSON via `slog`, written to stdout, level Info. Promtail scrapes them from the Docker socket — no file logging.
- No LLM, no API key, no persistence anywhere in this plan.
- Generated Go code lives in `chat-service/chat/` and is committed to git (the repo commits generated protobuf code).
- Windows host: the `Bash` tool runs Git Bash. Use forward slashes and POSIX syntax in commands.

---

### Task 1: Proto contract and generated bindings

Defines the wire contract and produces the Go and TypeScript bindings every later task imports. Nothing else can compile until this lands.

**Files:**
- Create: `proto/chat.proto`
- Create: `chat-service/go.mod`, `chat-service/go.sum`
- Generated: `chat-service/chat/chat.pb.go`, `chat-service/chat/chat_grpc.pb.go`
- Generated: `frontend/src/gen/chat_pb.ts`, `frontend/src/gen/chat_connect.ts`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - Go package `chatpb "chat-service/chat"` with `Message`, `ChatRequest`, `ChatChunk`, `Done`, enum constants `Role_ROLE_UNSPECIFIED` / `Role_ROLE_USER` / `Role_ROLE_ASSISTANT`, oneof wrappers `ChatChunk_TextDelta` and `ChatChunk_Done`, plus `ChatServiceServer`, `UnimplementedChatServiceServer`, `RegisterChatServiceServer`.
  - The generated server method signature, which Task 3 must match exactly:
    `Chat(*ChatRequest, grpc.ServerStreamingServer[ChatChunk]) error`
  - TypeScript: `ChatService` from `gen/chat_connect`, and `Role`, `ChatChunk`, `Done` from `gen/chat_pb`.

> **TypeScript enum naming gotcha:** `protoc-gen-es` strips the enum-name prefix from value names. Go gets `Role_ROLE_USER`; TypeScript gets `Role.USER`. Task 6 depends on this.

- [ ] **Step 1: Write the proto file**

Create `proto/chat.proto`:

```proto
syntax = "proto3";

package chat.v1;
option go_package = "./chat";

service ChatService {
  // One request, many responses: the server pushes ChatChunk frames until Done.
  rpc Chat(ChatRequest) returns (stream ChatChunk);
}

enum Role {
  ROLE_UNSPECIFIED = 0;
  ROLE_USER = 1;
  ROLE_ASSISTANT = 2;
}

message Message {
  Role role = 1;
  string content = 2;
}

message ChatRequest {
  // Full conversation history. The last entry is the new user turn.
  repeated Message messages = 1;
}

message ChatChunk {
  oneof event {
    string text_delta = 1;  // append to the in-progress assistant message
    Done done = 2;          // terminal frame
  }
}

message Done {
  string stop_reason = 1;
  int32 input_tokens = 2;   // 0 from the echo stub; real values in roadmap step 3
  int32 output_tokens = 3;
}
```

- [ ] **Step 2: Initialise the Go module**

```bash
cd chat-service
go mod init chat-service
go get google.golang.org/grpc@v1.81.1
go get google.golang.org/protobuf@v1.36.11
go get github.com/prometheus/client_golang@v1.23.2
go get go.opentelemetry.io/otel@v1.44.0
go get go.opentelemetry.io/otel/sdk@v1.44.0
go get go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc@v1.44.0
go get go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc@v0.69.0
```

Then open `chat-service/go.mod` and confirm the directive line reads `go 1.26.3`. If `go mod init` wrote a bare `go 1.26`, edit it to `1.26.3`.

- [ ] **Step 3: Generate the Go bindings**

From the repository root:

```bash
protoc -I proto --go_out=chat-service --go-grpc_out=chat-service proto/chat.proto
```

`option go_package = "./chat"` makes protoc write into a `chat/` subdirectory of the `--*_out` path, so this produces `chat-service/chat/chat.pb.go` and `chat-service/chat/chat_grpc.pb.go`.

> This is deliberately different from `order-service`, whose generated code is duplicated at both `orders/` and `order-service/orders/`. Do not create a second copy for chat.

- [ ] **Step 4: Verify the Go bindings compile and check the stream signature**

```bash
cd chat-service && go build ./...
```

Expected: no output (success).

```bash
grep -n "Chat(\*ChatRequest" chat-service/chat/chat_grpc.pb.go
```

Expected: a line containing `grpc.ServerStreamingServer[ChatChunk]`. If instead you see a generated `ChatService_ChatServer` interface, protoc-gen-go-grpc is older than v1.5 — upgrade it, because Task 3's handler signature assumes the generic form.

- [ ] **Step 5: Generate the TypeScript bindings**

```bash
cd frontend && npx buf generate ../proto
```

Expected: `frontend/src/gen/chat_pb.ts` and `frontend/src/gen/chat_connect.ts` created. The existing `service_pb.ts` / `service_connect.ts` get regenerated identically — that is fine.

- [ ] **Step 6: Verify the frontend still type-checks**

```bash
cd frontend && npx tsc -b
```

Expected: no errors.

- [ ] **Step 7: Commit**

```bash
git add proto/chat.proto chat-service/go.mod chat-service/go.sum chat-service/chat/ frontend/src/gen/
git commit -m "feat(proto): add chat.v1 ChatService server-streaming contract"
```

---

### Task 2: EchoResponder

The reply generator behind an interface. Pure logic, no gRPC — this is where the LLM will be swapped in during roadmap step 3.

**Files:**
- Create: `chat-service/responder.go`
- Test: `chat-service/responder_test.go`

**Interfaces:**
- Consumes: `chatpb.Message`, `chatpb.Role_ROLE_USER`, `chatpb.Role_ROLE_ASSISTANT` from Task 1.
- Produces:
  - `type Usage struct { StopReason string; InputTokens int32; OutputTokens int32 }`
  - `type Responder interface { Stream(ctx context.Context, history []*chatpb.Message, emit func(delta string) error) (Usage, error) }`
  - `type EchoResponder struct { Delay time.Duration }` implementing `Responder` on a pointer receiver — construct as `&EchoResponder{Delay: d}`.

- [ ] **Step 1: Write the failing tests**

Create `chat-service/responder_test.go`:

```go
package main

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	chatpb "chat-service/chat"
)

func userHistory(contents ...string) []*chatpb.Message {
	msgs := make([]*chatpb.Message, 0, len(contents))
	for _, c := range contents {
		msgs = append(msgs, &chatpb.Message{Role: chatpb.Role_ROLE_USER, Content: c})
	}
	return msgs
}

func TestEchoResponderChunking(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
	}{
		{"single word", "hello", []string{"hello"}},
		{"multiple words", "hello there world", []string{"hello", " there", " world"}},
		{"collapses extra whitespace", "  hello   there  ", []string{"hello", " there"}},
		{"empty content", "", nil},
		{"whitespace only", "   ", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &EchoResponder{}
			var got []string

			usage, err := r.Stream(context.Background(), userHistory(tt.content), func(d string) error {
				got = append(got, d)
				return nil
			})
			if err != nil {
				t.Fatalf("Stream() error = %v, want nil", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("deltas = %q, want %q", got, tt.want)
			}
			if usage.StopReason != "end_turn" {
				t.Errorf("StopReason = %q, want %q", usage.StopReason, "end_turn")
			}
			if int(usage.OutputTokens) != len(tt.want) {
				t.Errorf("OutputTokens = %d, want %d", usage.OutputTokens, len(tt.want))
			}
		})
	}
}

func TestEchoResponderUsesLastMessage(t *testing.T) {
	r := &EchoResponder{}
	history := []*chatpb.Message{
		{Role: chatpb.Role_ROLE_USER, Content: "first"},
		{Role: chatpb.Role_ROLE_ASSISTANT, Content: "reply"},
		{Role: chatpb.Role_ROLE_USER, Content: "second"},
	}

	var got []string
	if _, err := r.Stream(context.Background(), history, func(d string) error {
		got = append(got, d)
		return nil
	}); err != nil {
		t.Fatalf("Stream() error = %v, want nil", err)
	}

	if want := []string{"second"}; !slices.Equal(got, want) {
		t.Errorf("deltas = %q, want %q", got, want)
	}
}

func TestEchoResponderStopsOnCancel(t *testing.T) {
	r := &EchoResponder{Delay: 20 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	count := 0
	_, err := r.Stream(ctx, userHistory("one two three four five"), func(string) error {
		count++
		if count == 2 {
			cancel()
		}
		return nil
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Stream() error = %v, want context.Canceled", err)
	}
	if count != 2 {
		t.Errorf("emitted %d deltas, want 2 (should stop at cancel)", count)
	}
}

func TestEchoResponderPropagatesEmitError(t *testing.T) {
	r := &EchoResponder{}
	sentinel := errors.New("send failed")

	_, err := r.Stream(context.Background(), userHistory("one two"), func(string) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Stream() error = %v, want %v", err, sentinel)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd chat-service && go test ./... -run TestEchoResponder -v
```

Expected: FAIL — `undefined: EchoResponder`.

- [ ] **Step 3: Write the implementation**

Create `chat-service/responder.go`:

```go
package main

import (
	"context"
	"strings"
	"time"

	chatpb "chat-service/chat"
)

// Usage reports what a Responder consumed and produced for one turn.
// The echo stub leaves InputTokens at zero; a model-backed Responder fills
// all three fields from the provider's usage report.
type Usage struct {
	StopReason   string
	InputTokens  int32
	OutputTokens int32
}

// Responder produces an assistant reply for a conversation, emitting it in
// pieces as they become available. emit is called once per piece; if emit
// returns an error, Stream stops and returns that error unchanged.
//
// This is the seam for roadmap step 3: a Claude-backed implementation drops
// in here without the RPC handler changing.
type Responder interface {
	Stream(ctx context.Context, history []*chatpb.Message, emit func(delta string) error) (Usage, error)
}

// EchoResponder replays the last user message one word at a time. It stands in
// for a real model so the streaming transport can be exercised at zero cost.
type EchoResponder struct {
	// Delay is the pause before each word, imitating model latency.
	Delay time.Duration
}

func (e *EchoResponder) Stream(ctx context.Context, history []*chatpb.Message, emit func(delta string) error) (Usage, error) {
	words := strings.Fields(lastContent(history))

	for i, word := range words {
		select {
		case <-ctx.Done():
			return Usage{}, ctx.Err()
		case <-time.After(e.Delay):
		}

		// Re-join with single spaces: the client concatenates deltas verbatim,
		// so the separator has to travel with the word.
		delta := word
		if i > 0 {
			delta = " " + word
		}
		if err := emit(delta); err != nil {
			return Usage{}, err
		}
	}

	return Usage{
		StopReason:   "end_turn",
		OutputTokens: int32(len(words)),
	}, nil
}

func lastContent(history []*chatpb.Message) string {
	if len(history) == 0 {
		return ""
	}
	return history[len(history)-1].GetContent()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
cd chat-service && go test ./... -run TestEchoResponder -v
```

Expected: PASS for all four test functions.

- [ ] **Step 5: Commit**

```bash
git add chat-service/responder.go chat-service/responder_test.go
git commit -m "feat(chat-service): add Responder interface and echo stub"
```

---

### Task 3: Chat RPC handler and metrics

Wires a `Responder` to the gRPC stream, validates input, and records Prometheus metrics.

**Files:**
- Create: `chat-service/chat.go`
- Create: `chat-service/metrics.go`
- Create: `chat-service/log_trace.go`
- Test: `chat-service/chat_test.go`

**Interfaces:**
- Consumes: `Responder`, `Usage`, `EchoResponder` from Task 2; `chatpb` types from Task 1.
- Produces:
  - `type chatServer struct { chatpb.UnimplementedChatServiceServer; responder Responder }` — Task 4 constructs this as `&chatServer{responder: ...}`.
  - `func logWithTrace(ctx context.Context, base *slog.Logger) *slog.Logger`
  - Metric vars `chatStreamsTotal` (`*prometheus.CounterVec`, label `status`), `chatChunksSentTotal`, `chatStreamDuration`.

- [ ] **Step 1: Write the metrics and the trace-log helper**

These have no tests of their own — they are declarations the handler and its tests need in order to compile.

Create `chat-service/metrics.go`:

```go
package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	chatStreamsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chat_streams_total",
		Help: "Total chat streams by terminal status.",
	}, []string{"status"})

	chatChunksSentTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "chat_chunks_sent_total",
		Help: "Total ChatChunk text deltas sent to clients.",
	})

	chatStreamDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "chat_stream_duration_seconds",
		Help:    "Wall time of a chat stream from request to terminal frame.",
		Buckets: prometheus.DefBuckets,
	})
)
```

Create `chat-service/log_trace.go` — identical to `inventory-service/log_trace.go`:

```go
package main

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// logWithTrace returns a logger with trace_id and span_id fields from ctx.
// Returns base unchanged if no active span is in ctx.
func logWithTrace(ctx context.Context, base *slog.Logger) *slog.Logger {
	span := trace.SpanFromContext(ctx)
	if !span.SpanContext().IsValid() {
		return base
	}
	return base.With(
		"trace_id", span.SpanContext().TraceID().String(),
		"span_id", span.SpanContext().SpanID().String(),
	)
}
```

- [ ] **Step 2: Write the failing tests**

Create `chat-service/chat_test.go`:

```go
package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	chatpb "chat-service/chat"
)

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
```

- [ ] **Step 3: Run the tests to verify they fail**

```bash
cd chat-service && go test ./... -run TestChat -v
```

Expected: FAIL — `undefined: chatServer`.

- [ ] **Step 4: Write the handler**

Create `chat-service/chat.go`:

```go
package main

import (
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
		// A cancelled context means the client hung up — expected, not a fault.
		outcome := "error"
		if ctx.Err() != nil {
			outcome = "cancelled"
		}
		chatStreamsTotal.WithLabelValues(outcome).Inc()
		chatStreamDuration.Observe(time.Since(start).Seconds())
		log.Warn("chat stream ended early", "error", err, "outcome", outcome)
		return err
	}

	if err := stream.Send(&chatpb.ChatChunk{
		Event: &chatpb.ChatChunk_Done{Done: &chatpb.Done{
			StopReason:   usage.StopReason,
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
		}},
	}); err != nil {
		chatStreamsTotal.WithLabelValues("error").Inc()
		chatStreamDuration.Observe(time.Since(start).Seconds())
		return err
	}

	chatStreamsTotal.WithLabelValues("ok").Inc()
	chatStreamDuration.Observe(time.Since(start).Seconds())
	log.Info("chat stream completed", "output_tokens", usage.OutputTokens)
	return nil
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
```

- [ ] **Step 5: Run the full test suite to verify it passes**

```bash
cd chat-service && go test ./... -v
```

Expected: PASS for every `TestEchoResponder*` and `TestChat*` function.

- [ ] **Step 6: Commit**

```bash
git add chat-service/chat.go chat-service/metrics.go chat-service/log_trace.go chat-service/chat_test.go
git commit -m "feat(chat-service): add Chat streaming handler with metrics and validation"
```

---

### Task 4: Service bootstrap and container image

Turns the tested packages into a running process.

**Files:**
- Create: `chat-service/main.go`
- Create: `chat-service/telemetry.go`
- Create: `chat-service/Dockerfile`
- Create: `chat-service/.env.example`

**Interfaces:**
- Consumes: `chatServer` and `chatpb.RegisterChatServiceServer` from Task 3, `EchoResponder` from Task 2.
- Produces: a container listening for gRPC on `$GRPC_PORT` and serving `/metrics` on `:9091`. Task 5 wires it into compose, Envoy, and Prometheus.

- [ ] **Step 1: Write the tracer setup**

Create `chat-service/telemetry.go` — the `inventory-service` version with the service name changed:

```go
package main

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
)

func initTracer(ctx context.Context) (func(), error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "tempo:4317"
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("chat-service"),
		)),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return func() { tp.Shutdown(context.Background()) }, nil
}
```

- [ ] **Step 2: Write main.go**

Create `chat-service/main.go`:

```go
package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
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
	chatpb.RegisterChatServiceServer(grpcServer, &chatServer{
		responder: &EchoResponder{Delay: echoDelay()},
	})

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
```

- [ ] **Step 3: Write the env template and the Dockerfile**

Create `chat-service/.env.example`:

```
GRPC_PORT=:50051
OTEL_EXPORTER_OTLP_ENDPOINT=tempo:4317
# Pause before each echoed word, in milliseconds. 0 disables the delay.
ECHO_DELAY_MS=60
```

Create `chat-service/Dockerfile` — the `order-service` pattern, with generated code copied from inside the service directory:

```dockerfile
FROM golang:1.26 AS builder
WORKDIR /app

COPY chat-service/go.mod chat-service/go.sum ./
COPY chat-service/*.go ./
COPY chat-service/chat/ chat/

RUN CGO_ENABLED=0 GOOS=linux go build -o /chat-service .

FROM alpine:latest
COPY --from=builder /chat-service /chat-service
EXPOSE 50051
CMD ["/chat-service"]
```

- [ ] **Step 4: Verify the binary builds and the tests still pass**

```bash
cd chat-service && go build ./... && go vet ./... && go test ./...
```

Expected: no build or vet output, and `ok chat-service` from the tests.

- [ ] **Step 5: Verify the image builds**

From the repository root — note the build context is `.`, matching the other services:

```bash
docker build -f chat-service/Dockerfile -t chat-service:dev .
```

Expected: build succeeds, ending in a `naming to docker.io/library/chat-service:dev` line.

- [ ] **Step 6: Commit**

```bash
git add chat-service/main.go chat-service/telemetry.go chat-service/Dockerfile chat-service/.env.example
git commit -m "feat(chat-service): add service bootstrap, tracing, and Dockerfile"
```

---

### Task 5: Infrastructure wiring

Puts chat-service into the stack: compose, Envoy routing, Prometheus scraping.

**Files:**
- Modify: `docker-compose.yml`
- Modify: `envoy/envoy.yaml`
- Modify: `prometheus/prometheus.yml`

**Interfaces:**
- Consumes: the `chat-service` image from Task 4.
- Produces: `/chat.v1.ChatService/Chat` reachable through Envoy at `http://localhost:8080`, which Task 6's frontend calls.

- [ ] **Step 1: Add the compose service**

In `docker-compose.yml`, add after the `inventory-service` block:

```yaml
  chat-service:
    build:
      context: .
      dockerfile: chat-service/Dockerfile
    container_name: chat-service
    env_file:
      - chat-service/.env
    networks:
      - micro-network
```

No published port — the browser reaches it through Envoy, and Prometheus scrapes it over `micro-network`.

- [ ] **Step 2: Make Envoy wait for it**

In the same file, the `envoy` service currently has:

```yaml
    depends_on:
      - order-service
```

Change it to:

```yaml
    depends_on:
      - order-service
      - chat-service
```

- [ ] **Step 3: Create the local env file**

```bash
cp chat-service/.env.example chat-service/.env
```

`env_file` fails the whole `docker compose up` if the file is missing, and `.env` is not committed.

- [ ] **Step 4: Add the Envoy route**

In `envoy/envoy.yaml`, replace the entire `routes:` block:

```yaml
                      routes:
                        - match: { prefix: "/" }
                          route:
                            cluster: order_service_cluster
                            max_stream_duration:
                              grpc_timeout_header_max: 0s
```

with:

```yaml
                      routes:
                        - match: { prefix: "/chat.v1.ChatService/" }
                          route:
                            cluster: chat_service_cluster
                            max_stream_duration:
                              grpc_timeout_header_max: 0s
                        - match: { prefix: "/" }
                          route:
                            cluster: order_service_cluster
                            max_stream_duration:
                              grpc_timeout_header_max: 0s
```

> **Order is load-bearing.** Envoy takes the first matching route and `prefix: "/"` matches everything. If the chat route goes below it, chat requests silently reach order-service and fail as `UNIMPLEMENTED`. Keep the catch-all last.

The listener's `grpc_web` filter and CORS policy already apply to every route — no change needed there.

- [ ] **Step 5: Add the Envoy cluster**

In the same file, add to `clusters:` after `order_service_cluster`. A new cluster inherits nothing, so every option is repeated deliberately:

```yaml
    - name: chat_service_cluster
      connect_timeout: 0.25s
      type: STRICT_DNS
      http2_protocol_options: {}
      lb_policy: round_robin
      load_assignment:
        cluster_name: chat_service_cluster
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address:
                      address: chat-service
                      port_value: 50051
```

`http2_protocol_options` is required because Go gRPC only speaks HTTP/2. `grpc_timeout_header_max: 0s` on the route above means no timeout ceiling — irrelevant for a 200ms echo, required once a model generates for 30 seconds.

- [ ] **Step 6: Add the Prometheus scrape job**

In `prometheus/prometheus.yml`, add after the `inventory-service` job:

```yaml
  - job_name: chat-service
    static_configs:
      - targets: ['chat-service:9091']
```

Static, matching `inventory-service`. Switch to `docker_sd_configs` (copying the `order-service` job) if chat-service ever gets scaled to multiple replicas.

- [ ] **Step 7: Bring the stack up and verify routing end to end**

```bash
docker compose up -d --build
docker compose logs chat-service --tail 20
```

Expected: a JSON line `{"level":"INFO","msg":"chat service listening","port":":50051"}`.

Verify Envoy routes to the right backend. Send a real (if empty) grpc-web frame — the 5-byte length-prefixed header the browser sends:

```bash
printf '\x00\x00\x00\x00\x00' | curl -s -D - -o /dev/null --max-time 10 \
  -H 'Content-Type: application/grpc-web+proto' \
  --data-binary @- http://localhost:8080/chat.v1.ChatService/Chat
```

Expected: `HTTP/1.1 200 OK` with `grpc-status: 3` and `grpc-message: messages must not be empty` — the Task 3 validation answering through Envoy, which proves the whole path.

> Do **not** send a zero-byte body here. Envoy's `grpc_web` filter hangs on a genuinely empty body for any real method, on the pre-existing `order-service` routes too — it is not a chat-service fault, but it makes the check look like a routing failure.

Now confirm the catch-all still works and that the two routes are actually distinct:

```bash
curl -s -D - -o /dev/null \
  -H 'Content-Type: application/grpc-web+proto' \
  -X POST http://localhost:8080/chat.v1.ChatService/DoesNotExist | grep -i grpc-status
```

Expected: a `grpc-status: 12` header (UNIMPLEMENTED) — proof the request reached a real gRPC server rather than being swallowed.

- [ ] **Step 8: Verify Prometheus discovered the target**

Open `http://localhost:9090/targets`. Expected: job `chat-service`, endpoint `chat-service:9091/metrics`, state **UP**.

- [ ] **Step 9: Commit**

```bash
git add docker-compose.yml envoy/envoy.yaml prometheus/prometheus.yml
git commit -m "feat(infra): route chat.v1.ChatService through Envoy, scrape chat-service"
```

---

### Task 6: Frontend chat panel

Consumes the stream and renders it incrementally, and splits `MainPage` so the two features do not share a file.

**Files:**
- Create: `frontend/src/chatClient.ts`
- Create: `frontend/src/components/chat/useChatStream.ts`
- Create: `frontend/src/components/chat/ChatPanel.tsx`
- Create: `frontend/src/components/chat/ChatPanel.css`
- Create: `frontend/src/components/packet-sender/PacketSender.tsx`
- Create: `frontend/src/components/packet-sender/PacketSender.css`
- Move: `frontend/src/components/main-page/usePacketSender.ts` → `frontend/src/components/packet-sender/usePacketSender.ts`
- Modify: `frontend/src/components/main-page/MainPage.tsx` (replaced wholesale)
- Modify: `frontend/src/components/main-page/MainPage.css:1-85` (remove `.packet-form` and `.response`, add `.main-layout`)

**Interfaces:**
- Consumes: `ChatService` from `gen/chat_connect`, `Role` from `gen/chat_pb` (Task 1).
- Produces: nothing downstream — this is the last task.

> The generated TypeScript oneof is a discriminated union: `chunk.event` is `{case: "textDelta", value: string} | {case: "done", value: Done} | {case: undefined}`. And remember `protoc-gen-es` strips the enum prefix, so it is `Role.USER`, not `Role.ROLE_USER`.

- [ ] **Step 1: Add the chat transport**

Create `frontend/src/chatClient.ts`:

```ts
import { createPromiseClient } from '@connectrpc/connect';
import { createGrpcWebTransport } from '@connectrpc/connect-web';
import { ChatService } from './gen/chat_connect';

// Same Envoy listener as the packet client — Envoy demuxes on the gRPC path,
// so no second port is involved.
const transport = createGrpcWebTransport({
  baseUrl: 'http://localhost:8080',
});

export const chatClient = createPromiseClient(ChatService, transport);
```

- [ ] **Step 2: Write the streaming hook**

Create `frontend/src/components/chat/useChatStream.ts`:

```ts
import { useCallback, useEffect, useRef, useState } from 'react';
import { chatClient } from '../../chatClient';
import { Role } from '../../gen/chat_pb';

export interface ChatMessage {
  role: Role;
  content: string;
}

// The server is stateless, so the client owns the history. Cap it so a long
// conversation does not grow the request without bound.
const MAX_HISTORY = 20;

function useChatStream() {
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [streaming, setStreaming] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const abortRef = useRef<AbortController | null>(null);

  useEffect(() => () => abortRef.current?.abort(), []);

  const stop = useCallback(() => {
    abortRef.current?.abort();
  }, []);

  const send = useCallback(
    async (text: string) => {
      const trimmed = text.trim();
      if (!trimmed || streaming) return;

      const history: ChatMessage[] = [...messages, { role: Role.USER, content: trimmed }];
      // Append an empty assistant message that the deltas accumulate into.
      setMessages([...history, { role: Role.ASSISTANT, content: '' }]);
      setStreaming(true);
      setError(null);

      const ac = new AbortController();
      abortRef.current = ac;

      try {
        const stream = chatClient.chat(
          { messages: history.slice(-MAX_HISTORY) },
          { signal: ac.signal }
        );

        for await (const chunk of stream) {
          if (chunk.event.case !== 'textDelta') continue;
          const delta = chunk.event.value;
          setMessages((prev) => {
            const next = [...prev];
            const last = next[next.length - 1];
            next[next.length - 1] = { ...last, content: last.content + delta };
            return next;
          });
        }
      } catch (err) {
        // An aborted stream is the user pressing Stop, not a failure.
        if (!ac.signal.aborted) {
          setError(err instanceof Error ? err.message : 'Unknown error');
        }
      } finally {
        setStreaming(false);
        abortRef.current = null;
      }
    },
    [messages, streaming]
  );

  return { messages, streaming, error, send, stop };
}

export default useChatStream;
```

The `done` frame needs no branch — the loop ends when the server closes the stream, and `finally` clears `streaming`. It carries token counts that roadmap step 3 will start displaying.

- [ ] **Step 3: Write the panel and its styles**

Create `frontend/src/components/chat/ChatPanel.tsx`:

```tsx
import { useState } from 'react';
import useChatStream from './useChatStream';
import { Role } from '../../gen/chat_pb';
import './ChatPanel.css';

function ChatPanel() {
  const { messages, streaming, error, send, stop } = useChatStream();
  const [input, setInput] = useState('');

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    send(input);
    setInput('');
  }

  return (
    <section className="chat-panel">
      <h1>Chat</h1>

      <ol className="chat-messages">
        {messages.map((m, i) => (
          <li key={i} className={m.role === Role.USER ? 'chat-user' : 'chat-assistant'}>
            {m.content}
            {streaming && i === messages.length - 1 && <span className="chat-cursor">▌</span>}
          </li>
        ))}
      </ol>

      {error && <p className="chat-error">{error}</p>}

      <form className="chat-form" onSubmit={handleSubmit}>
        <input
          type="text"
          value={input}
          onChange={(e) => setInput(e.target.value)}
          placeholder="Say something"
          disabled={streaming}
        />
        {streaming ? (
          <button type="button" onClick={stop}>
            Stop
          </button>
        ) : (
          <button type="submit" disabled={!input.trim()}>
            Send
          </button>
        )}
      </form>
    </section>
  );
}

export default ChatPanel;
```

The list is append-only, so the array index is a stable key here.

Create `frontend/src/components/chat/ChatPanel.css`:

```css
.chat-panel {
  display: flex;
  flex-direction: column;
  gap: 16px;
  width: 100%;
  max-width: 480px;
  text-align: left;
}

.chat-messages {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 10px;
  min-height: 200px;
  max-height: 420px;
  overflow-y: auto;

  li {
    padding: 10px 14px;
    border-radius: 6px;
    font-size: 15px;
    white-space: pre-wrap;
    max-width: 85%;
  }

  .chat-user {
    align-self: flex-end;
    background: var(--accent-bg);
    color: var(--text-h);
  }

  .chat-assistant {
    align-self: flex-start;
    background: var(--code-bg);
    border: 1px solid var(--border);
    color: var(--text);
  }
}

.chat-cursor {
  opacity: 0.6;
}

.chat-error {
  margin: 0;
  font-size: 14px;
  color: #991b1b;

  @media (prefers-color-scheme: dark) {
    color: #fca5a5;
  }
}

.chat-form {
  display: flex;
  gap: 8px;

  input {
    flex: 1 1 auto;
    font-size: 16px;
    padding: 8px 12px;
    border-radius: 6px;
    border: 1px solid var(--border);
    background: var(--code-bg);
    color: var(--text-h);
    font-family: var(--sans);
    outline: none;

    &:focus {
      border-color: var(--accent);
    }
  }

  button {
    font-size: 16px;
    padding: 10px 20px;
    border-radius: 6px;
    border: none;
    background: var(--accent);
    color: #fff;
    cursor: pointer;
    transition: opacity 0.2s;

    &:hover:not(:disabled) {
      opacity: 0.85;
    }

    &:disabled {
      opacity: 0.5;
      cursor: not-allowed;
    }
  }
}
```

- [ ] **Step 4: Extract the packet sender**

Move the hook, preserving history:

```bash
git mv frontend/src/components/main-page/usePacketSender.ts frontend/src/components/packet-sender/usePacketSender.ts
```

Create `frontend/src/components/packet-sender/PacketSender.tsx` — the current `MainPage.tsx` body verbatim, renamed, with the CSS import repointed:

```tsx
import usePacketSender from './usePacketSender';
import './PacketSender.css';

function PacketSender() {
  const { form, handleChange, handleSend, busy, result, error, isStreaming, packetCount } =
    usePacketSender();

  return (
    <section id="center">
      <h1>Send Packet</h1>
      <form className="packet-form" onSubmit={handleSend}>
        <label>
          Client ID
          <input
            name="clientId"
            type="text"
            value={form.clientId}
            onChange={handleChange}
            placeholder="client-001"
            required
          />
        </label>
        <label>
          Payload
          <input
            name="payload"
            type="text"
            value={form.payload}
            onChange={handleChange}
            placeholder="hello world"
            required
          />
        </label>
        <label>
          Sequence Number
          <input
            name="sequenceNumber"
            type="number"
            value={form.sequenceNumber}
            onChange={handleChange}
            min={0}
            required
          />
        </label>
        <label>
          Packet Count
          <input
            name="packetCount"
            type="number"
            value={form.packetCount}
            onChange={handleChange}
            min={1}
            required
          />
        </label>
        <button type="submit" disabled={busy}>
          {busy
            ? isStreaming
              ? 'Streaming…'
              : 'Sending…'
            : `Send${packetCount > 1 ? ` ${packetCount} Packets` : ' Packet'}`}
        </button>
      </form>

      {result && (
        <div className={`response ${result.success ? 'success' : 'failure'}`}>
          <strong>{result.success ? 'Success' : 'Failed'}</strong>
          <span>{result.message}</span>
        </div>
      )}

      {error && (
        <div className="response failure">
          <strong>Error</strong>
          <span>{error}</span>
        </div>
      )}
    </section>
  );
}

export default PacketSender;
```

Create `frontend/src/components/packet-sender/PacketSender.css` by cutting `MainPage.css:1-85` — the `.packet-form` and `.response` rules — into it verbatim. Then delete those same two rule blocks from `MainPage.css`. Leave everything else in `MainPage.css` alone; the `.hero`, `#next-steps`, `#docs`, `#spacer`, `.ticks`, and `.counter` rules are unused Vite-template leftovers and are out of scope.

- [ ] **Step 5: Turn MainPage into a two-panel host**

Replace the whole of `frontend/src/components/main-page/MainPage.tsx` with:

```tsx
import PacketSender from '../packet-sender/PacketSender';
import ChatPanel from '../chat/ChatPanel';
import './MainPage.css';

function MainPage() {
  return (
    <div className="main-layout">
      <PacketSender />
      <ChatPanel />
    </div>
  );
}

export default MainPage;
```

Add to `MainPage.css` (the `#center` rule it already contains styles the packet-sender section, so leave it):

```css
.main-layout {
  display: flex;
  align-items: flex-start;
  justify-content: center;
  gap: 48px;
  padding: 32px 20px;

  @media (max-width: 1024px) {
    flex-direction: column;
    align-items: center;
    gap: 32px;
  }
}
```

- [ ] **Step 6: Verify types and lint pass**

```bash
cd frontend && npx tsc -b && npm run lint
```

Expected: no errors from either.

- [ ] **Step 7: Verify end to end in the browser**

With the stack from Task 5 still running:

```bash
cd frontend && npm run dev
```

Open the dev server URL and check each of these:

1. Type `hello there world` and press Send. The reply appears **word by word**, not all at once — that is the stream working. If the whole reply lands in one paint, Envoy is buffering and the route's `grpc_timeout_header_max` or the cluster's `http2_protocol_options` is wrong.
2. Send a long message and press **Stop** partway. Output halts, the partial text stays, and no error appears.
3. The packet-sender form still sends and still shows its success box — the Envoy catch-all is intact.
4. `docker compose logs chat-service` shows `chat stream started` / `chat stream completed` JSON lines carrying `trace_id`.
5. `curl -s http://localhost:9091/metrics | grep chat_` inside the network, or Prometheus at `http://localhost:9090` querying `chat_streams_total` — expect a series with `status="ok"`, and one with `status="cancelled"` after step 2.
6. Grafana Explore → Tempo, search service `chat-service`. Expect a span named for the `Chat` method carrying `chat.history_len`.

- [ ] **Step 8: Commit**

```bash
git add frontend/src/chatClient.ts frontend/src/components/chat/ frontend/src/components/packet-sender/ frontend/src/components/main-page/
git commit -m "feat(frontend): add streaming chat panel, split MainPage into two panels"
```

---

## Verification Summary

After Task 6, all of the following must hold:

- `cd chat-service && go test ./...` passes.
- `cd frontend && npx tsc -b && npm run lint` passes.
- `docker compose up -d --build` brings up `chat-service` alongside the existing services.
- Prometheus `http://localhost:9090/targets` shows `chat-service` UP.
- A message sent from the browser renders word by word, and Stop halts it mid-stream.
- `chat_streams_total{status="ok"}` and `chat_streams_total{status="cancelled"}` both have values.
- The existing packet-sender path is unchanged.

## What Roadmap Step 3 Inherits

Adding Claude means: a new `chat-service/claude.go` implementing `Responder`, one constructor change in `main.go`, real values in the `Usage` struct, `ANTHROPIC_API_KEY` in the env file, and new token/cost metrics in `metrics.go`. The proto, the handler, Envoy, and the entire frontend stay as they are.
