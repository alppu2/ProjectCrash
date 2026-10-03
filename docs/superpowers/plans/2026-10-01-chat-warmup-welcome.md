# Chat Warmup and Welcome Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Warm the assistant when the page loads, show a loading state meanwhile, then reveal a fixed welcome message that tells visitors what the chat is for.

**Architecture:** A new unary `ChatService.Warmup` RPC. In llm mode, `RetrievingResponder.Warmup` makes one embedding call and one `max_tokens: 1` completion behind a `warmCache` (single-flight, 20-minute freshness, failures not cached). Echo mode has no warmer and succeeds immediately. The frontend calls `Warmup` on mount through a new `useWarmup` hook (`warming` → `ready` / `unavailable`), and `ChatPanel` renders the welcome from `welcome.ts` once ready.

**Tech Stack:** Go 1.26, grpc-go, Prometheus client_golang, protoc + protoc-gen-go/-go-grpc; React 19, TypeScript, Connect-ES v1, Vitest + Testing Library.

**Spec:** `docs/superpowers/specs/2026-10-01-chat-warmup-welcome-design.md`

## Global Constraints

- Freshness window: **20 minutes** (below compose's `OLLAMA_KEEP_ALIVE: 30m`).
- Server-side warmup timeout: **120s**, detached from the caller's context.
- Warmup completion: non-streaming, `max_tokens: 1`, same request shape as `condense`.
- Warm state is global per chat-service process, in memory; the client stores nothing.
- No new Go dependencies (no `golang.org/x/sync`).
- Metrics: `chat_warmups_total{outcome}` with `ok` | `error` | `cancelled`, and `chat_warmup_duration_seconds`; recorded from one deferred func in the handler.
- A warmup error reaches the browser as `codes.Unavailable` with a generic message; provider detail goes to the log only.
- Frontend styling stays plain markup + class names; the component library comes later.
- Comments follow CLAUDE.md: why, not what; three lines max.
- Each Go module is built and tested from its own directory (`cd chat-service`).

## Deviations from the spec

Decided while planning, against the code as it stands:

1. **The welcome lives in `useWarmup`, not in `useChatStream`'s `messages`.** `ChatPanel` renders it as its own first `<li>`. That keeps it out of the request history by construction, rather than relying on `trimHistory` dropping it, and leaves the existing `useChatStream` tests untouched.
2. **Sending is gated by `ChatPanel`, not by `send()`.** The input and Send button are disabled until `ready`. `useChatStream` does not change.
3. **The client timeout is 130s, not 120s.** That is above the server's 120s, so a cold load that is too slow fails on the server with a logged reason, not as a bare client deadline.
4. **Addition:** a server-side timeout is reported as `error`, not `cancelled`. `ClassifyOutcome` maps `context.DeadlineExceeded` to `cancelled`, which would hide a wedged provider as a client hangup.

## Review Focus

1. **A cold load that exceeds the 120s server timeout.** It must count as `outcome="error"` and reach the browser as Unavailable, not as a client cancel. Pinned in Task 2.
2. **A `max_tokens: 1` reply with empty content.** That is normal for some providers, and it must count as warm, not as a failure. Pinned in Task 3.
3. **A hosted provider's error body carrying account detail.** For example, a 401 that echoes the key. It must never reach the browser. Pinned in Task 4.
4. **React StrictMode's discarded first mount rejecting late.** That rejection must not flip a healthy assistant to `unavailable`. Pinned in Task 5.
5. **A message sent right after the welcome.** The request must carry only the user's turn, never the welcome. Pinned in Task 6.

---

### Task 1: `Warmup` RPC contract and codegen

**Files:**
- Modify: `proto/chat.proto`
- Regenerate: `chat-service/chat/chat.pb.go`, `chat-service/chat/chat_grpc.pb.go`, `frontend/src/gen/chat_pb.ts`, `frontend/src/gen/chat_connect.ts`

**Interfaces:**
- Produces: `chatpb.WarmupRequest`, `chatpb.WarmupResponse`, the `ChatServiceServer.Warmup(context.Context, *chatpb.WarmupRequest) (*chatpb.WarmupResponse, error)` method (served by `UnimplementedChatServiceServer` until Task 4), the TS classes `WarmupRequest` / `WarmupResponse` in `frontend/src/gen/chat_pb.ts`, and `chatClient.warmup(req, opts): Promise<WarmupResponse>`.

- [ ] **Step 1: Add the RPC and messages**

In `proto/chat.proto`, change the service and append the messages:

```proto
service ChatService {
  rpc Chat(ChatRequest) returns (stream ChatChunk);
  // Readies the provider before the first turn. Cheap to repeat: the server
  // shares one in-flight call and reuses a recent success.
  rpc Warmup(WarmupRequest) returns (WarmupResponse);
}
```

```proto
// Empty for now; room for e.g. a model id without a new RPC.
message WarmupRequest {}
message WarmupResponse {}
```

- [ ] **Step 2: Regenerate Go**

Run from the repo root:

```bash
cd proto && protoc --go_out=../chat-service --go-grpc_out=../chat-service chat.proto
```

Expected: no output. `git diff --stat chat-service/chat` shows both files changed. The header must still say `source: chat.proto`.

- [ ] **Step 3: Regenerate TypeScript**

Run from the repo root (the buf plugins are root devDependencies):

```bash
cd frontend && PATH="$(cd .. && pwd)/node_modules/.bin:$PATH" buf generate ../proto
```

Expected: `chat_pb.ts` gains `WarmupRequest` / `WarmupResponse`, and `chat_connect.ts` gains a `warmup` method of `MethodKind.Unary`. `service_*.ts` may show line-ending-only changes. Revert them with `git checkout -- src/gen/service_pb.ts src/gen/service_connect.ts` if `git diff --ignore-cr-at-eol --stat` shows no content change.

- [ ] **Step 4: Verify both sides build**

```bash
cd chat-service && go build ./... && go test ./...
cd ../frontend && npm run build
```

Expected: both succeed. Envoy needs no change, because the `/chat.v1.ChatService/` prefix route already covers `/chat.v1.ChatService/Warmup`.

- [ ] **Step 5: Commit**

```bash
git add proto/chat.proto chat-service/chat frontend/src/gen/chat_pb.ts frontend/src/gen/chat_connect.ts
git commit -m "feat(proto): add ChatService.Warmup"
```

---

### Task 2: `warmCache` — single-flight with a freshness window

**Files:**
- Create: `chat-service/internal/responder/warmup.go`
- Test: `chat-service/internal/responder/warmup_test.go`

**Interfaces:**
- Produces:
  - `type Warmer interface { Warmup(ctx context.Context) error }` (exported; the handler type-asserts on it)
  - `type warmCache struct` whose zero value is ready to use, with test hooks `now func() time.Time` and `timeout time.Duration`
  - `func (c *warmCache) do(ctx context.Context, fn func(context.Context) error) error`

- [ ] **Step 1: Write the failing tests**

`chat-service/internal/responder/warmup_test.go`:

```go
package responder

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Every page load calls Warmup. Without sharing, N visitors during a cold
// load would queue N completions on one GPU.
func TestWarmCacheSharesOneFlight(t *testing.T) {
	var c warmCache
	var calls atomic.Int32
	release := make(chan struct{})
	fn := func(context.Context) error {
		calls.Add(1)
		<-release
		return nil
	}

	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- c.do(context.Background(), fn)
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("do() error = %v, want nil", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("fn ran %d times, want 1", got)
	}
}

// Fresh successes are free; stale ones must warm again or the cache vouches
// for a model Ollama has unloaded.
func TestWarmCacheReusesFreshSuccessOnly(t *testing.T) {
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	c := warmCache{now: func() time.Time { return now }}
	calls := 0
	fn := func(context.Context) error { calls++; return nil }

	_ = c.do(context.Background(), fn)
	now = now.Add(warmFreshness - time.Second)
	_ = c.do(context.Background(), fn)
	if calls != 1 {
		t.Fatalf("calls within the window = %d, want 1", calls)
	}

	now = now.Add(2 * time.Second)
	_ = c.do(context.Background(), fn)
	if calls != 2 {
		t.Errorf("calls after the window = %d, want 2", calls)
	}
}

// A provider that recovers must be picked up by the next page load, not
// after a restart.
func TestWarmCacheDoesNotCacheFailure(t *testing.T) {
	var c warmCache
	results := []error{errors.New("provider down"), nil}
	calls := 0
	fn := func(context.Context) error { err := results[calls]; calls++; return err }

	if err := c.do(context.Background(), fn); err == nil {
		t.Fatal("first do() error = nil, want the provider error")
	}
	if err := c.do(context.Background(), fn); err != nil {
		t.Errorf("second do() error = %v, want nil", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

// A visitor closing the tab mid cold load must not abort the load every
// other visitor is waiting on.
func TestWarmCacheCallerCancelDoesNotCancelSharedWork(t *testing.T) {
	var c warmCache
	release := make(chan struct{})
	finished := make(chan error, 1)
	fn := func(ctx context.Context) error {
		<-release
		finished <- ctx.Err()
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.do(ctx, fn) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller got %v, want context.Canceled", err)
	}

	close(release)
	if err := <-finished; err != nil {
		t.Errorf("shared work saw ctx.Err() = %v, want nil", err)
	}
	// The flight finished successfully, so the next caller is served from cache.
	if err := c.do(context.Background(), func(context.Context) error {
		t.Error("fn ran again after a successful flight")
		return nil
	}); err != nil {
		t.Errorf("do() after flight error = %v, want nil", err)
	}
}

// A wedged provider must show up as an error, not as a client hangup:
// ClassifyOutcome maps DeadlineExceeded to "cancelled".
func TestWarmCacheTimeoutIsAnError(t *testing.T) {
	c := warmCache{timeout: 10 * time.Millisecond}
	err := c.do(context.Background(), func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if err == nil {
		t.Fatal("do() error = nil, want a timeout error")
	}
	if got := ClassifyOutcome(err); got != "error" {
		t.Errorf("ClassifyOutcome(%v) = %q, want %q", err, got, "error")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd chat-service && go test ./internal/responder/ -run TestWarmCache`
Expected: FAIL to compile with `undefined: warmCache` and `undefined: warmFreshness`.

- [ ] **Step 3: Implement**

`chat-service/internal/responder/warmup.go`:

```go
package responder

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Warmer readies a Responder's provider before the first turn. A Responder
// that does not implement it has nothing to warm.
type Warmer interface {
	Warmup(ctx context.Context) error
}

// Below compose's OLLAMA_KEEP_ALIVE (30m), so a fresh entry never vouches for
// a model Ollama has already unloaded.
const warmFreshness = 20 * time.Minute

// Covers a cold VRAM load (~33s) with headroom, like newLLMClient's header wait.
const warmTimeout = 120 * time.Second

type warmFlight struct {
	done chan struct{}
	err  error
}

// warmCache shares one in-flight warmup among concurrent callers and reuses a
// success for warmFreshness. Failures are not cached. The zero value is ready.
type warmCache struct {
	now     func() time.Time // nil means time.Now
	timeout time.Duration    // zero means warmTimeout

	mu       sync.Mutex
	warmedAt time.Time
	flight   *warmFlight
}

func (c *warmCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// do runs fn unless a fresh success or an in-flight call already covers it.
// fn is detached from ctx: one caller leaving must not abort the others' wait.
func (c *warmCache) do(ctx context.Context, fn func(context.Context) error) error {
	c.mu.Lock()
	if !c.warmedAt.IsZero() && c.clock().Sub(c.warmedAt) < warmFreshness {
		c.mu.Unlock()
		return nil
	}
	f := c.flight
	if f == nil {
		f = &warmFlight{done: make(chan struct{})}
		c.flight = f
		go c.run(context.WithoutCancel(ctx), f, fn)
	}
	c.mu.Unlock()

	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *warmCache) run(ctx context.Context, f *warmFlight, fn func(context.Context) error) {
	timeout := c.timeout
	if timeout == 0 {
		timeout = warmTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	err := fn(ctx)
	if err != nil && ctx.Err() != nil {
		// %v, not %w: ClassifyOutcome would read DeadlineExceeded as a hangup.
		err = fmt.Errorf("warmup exceeded %s: %v", timeout, err)
	}

	c.mu.Lock()
	if err == nil {
		c.warmedAt = c.clock()
	}
	c.flight = nil
	c.mu.Unlock()

	f.err = err
	close(f.done)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd chat-service && go test -race ./internal/responder/ -run TestWarmCache -count=3`
Expected: PASS. `-race` matters here: the flight result is published through `close(f.done)`. If the race detector is unavailable on Windows (it needs cgo), run without `-race` and note that.

- [ ] **Step 5: Commit**

```bash
git add chat-service/internal/responder/warmup.go chat-service/internal/responder/warmup_test.go
git commit -m "feat(chat): single-flight warmup cache with a freshness window"
```

---

### Task 3: Provider warmup — `OpenAIResponder.warm` and `RetrievingResponder.Warmup`

**Files:**
- Modify: `chat-service/internal/responder/openai.go` (the `condense` function, about lines 285–335)
- Modify: `chat-service/internal/responder/retrieve.go` (interfaces at about lines 14–41)
- Modify: `chat-service/internal/responder/new.go` (`newRetriever`'s return literal)
- Test: `chat-service/internal/responder/openai_test.go`, `chat-service/internal/responder/retrieve_test.go`, `chat-service/main_test.go`

**Interfaces:**
- Consumes: `warmCache.do` and `Warmer` from Task 2.
- Produces:
  - `func (o *OpenAIResponder) complete(ctx context.Context, op string, maxTokens int, msgs []openAIMessage) (string, error)`, which `condense` now uses
  - `func (o *OpenAIResponder) warm(ctx context.Context) error`
  - `type warmer interface { warm(ctx context.Context) error }`
  - a new field `RetrievingResponder.Warm warmer`
  - `func (r *RetrievingResponder) Warmup(ctx context.Context) error`, so `*RetrievingResponder` satisfies `Warmer`

- [ ] **Step 1: Write the failing tests**

Append to `chat-service/internal/responder/openai_test.go`. Check its imports and add `encoding/json` and `strings` if missing:

```go
// One token through the same request shape condense uses, so any provider
// that serves chat also serves warmup. An empty reply still counts as warm:
// max_tokens 1 often yields only a stop.
func TestOpenAIResponderWarmRequestShape(t *testing.T) {
	var got struct {
		Model     string `json:"model"`
		Stream    bool   `json:"stream"`
		MaxTokens int    `json:"max_tokens"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decoding body: %v", err)
		}
		w.Write([]byte(`{"choices":[{"message":{"content":""}}]}`))
	}))
	t.Cleanup(srv.Close)

	o := &OpenAIResponder{BaseURL: srv.URL, Model: "llama3.2:3b", Client: srv.Client()}
	if err := o.warm(context.Background()); err != nil {
		t.Fatalf("warm() error = %v, want nil", err)
	}
	if got.Model != "llama3.2:3b" || got.Stream || got.MaxTokens != 1 {
		t.Errorf("request = %+v, want model llama3.2:3b, stream false, max_tokens 1", got)
	}
}

// A bad key or missing model must fail warmup, so the page says unavailable
// instead of welcoming visitors to a chat that cannot answer.
func TestOpenAIResponderWarmSurfacesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"invalid api key"}}`, http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	o := &OpenAIResponder{BaseURL: srv.URL, Model: "m", Client: srv.Client()}
	err := o.warm(context.Background())
	if err == nil || !strings.Contains(err.Error(), "warmup returned HTTP 401") {
		t.Errorf("warm() error = %v, want one naming HTTP 401", err)
	}
}
```

Append to `chat-service/internal/responder/retrieve_test.go`:

```go
type countingEmbedder struct {
	fakeEmbedder
	calls int
}

func (c *countingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	c.calls++
	return c.fakeEmbedder.Embed(ctx, texts)
}

type fakeWarm struct {
	err   error
	calls int
}

func (f *fakeWarm) warm(context.Context) error {
	f.calls++
	return f.err
}

// Both models must be resident before the welcome, or the first question
// still pays a cold load on whichever was skipped.
func TestWarmupWarmsEmbedderAndModelOnce(t *testing.T) {
	emb := &countingEmbedder{}
	model := &fakeWarm{}
	r := &RetrievingResponder{Embedder: emb, Warm: model}

	for range 2 {
		if err := r.Warmup(context.Background()); err != nil {
			t.Fatalf("Warmup() error = %v, want nil", err)
		}
	}
	if emb.calls != 1 || model.calls != 1 {
		t.Errorf("embed, model calls = %d, %d; want 1, 1 (second Warmup served from cache)", emb.calls, model.calls)
	}
}

func TestWarmupFailsWhenEitherSideFails(t *testing.T) {
	cases := map[string]*RetrievingResponder{
		"embedder": {Embedder: fakeEmbedder{err: errors.New("embed down")}, Warm: &fakeWarm{}},
		"model":    {Embedder: fakeEmbedder{}, Warm: &fakeWarm{err: errors.New("model down")}},
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			if err := r.Warmup(context.Background()); err == nil {
				t.Error("Warmup() error = nil, want the failure")
			}
		})
	}
}
```

In `chat-service/main_test.go`, inside `TestNewResponderConfiguresTheGroundedResponder`, after the `o, ok := retriever.Inner.(*responder.OpenAIResponder)` block, add:

```go
	// Warmup must load the same model the turns use, or the welcome lies.
	if w, ok := retriever.Warm.(*responder.OpenAIResponder); !ok || w != o {
		t.Errorf("Warm = %T, want the same *responder.OpenAIResponder as Inner", retriever.Warm)
	}
	if _, ok := r.(responder.Warmer); !ok {
		t.Errorf("responder = %T, does not implement responder.Warmer", r)
	}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd chat-service && go test ./...`
Expected: FAIL to compile with `o.warm undefined`, `unknown field Warm`, and `r.Warmup undefined`.

- [ ] **Step 3: Extract `complete` from `condense`, add `warm`**

In `chat-service/internal/responder/openai.go`, replace the whole `condense` function with:

```go
// condense runs a non-streaming completion against the same model. It lands
// ahead of the first token, so its latency is measured separately.
func (o *OpenAIResponder) condense(ctx context.Context, msgs []*chatpb.Message) (string, error) {
	payload := append([]openAIMessage{{Role: "system", Content: condensePrompt}}, toOpenAIMessages(msgs)...)
	out, err := o.complete(ctx, "condense", condenseMaxTokens, payload)
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", fmt.Errorf("condense returned no content")
	}
	return out, nil
}

// warm loads the model on a local provider, and on a hosted one proves the
// URL, key, model and quota. The one-token reply is discarded.
func (o *OpenAIResponder) warm(ctx context.Context) error {
	_, err := o.complete(ctx, "warmup", 1, []openAIMessage{{Role: "user", Content: "ping"}})
	return err
}

// complete runs one non-streaming completion and returns the first choice's
// content, which may be empty. op prefixes every error.
func (o *OpenAIResponder) complete(ctx context.Context, op string, maxTokens int, msgs []openAIMessage) (string, error) {
	body, err := json.Marshal(struct {
		Model     string          `json:"model"`
		Stream    bool            `json:"stream"`
		MaxTokens int             `json:"max_tokens"`
		Messages  []openAIMessage `json:"messages"`
	}{Model: o.Model, Stream: false, MaxTokens: maxTokens, Messages: msgs})
	if err != nil {
		return "", fmt.Errorf("encoding %s request: %w", op, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("building %s request: %w", op, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.APIKey)
	}

	resp, err := o.Client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", fmt.Errorf("%s request failed: %w", op, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s returned HTTP %d: %s", op, resp.StatusCode, readErrorBody(resp.Body))
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decoding %s response: %w", op, err)
	}
	if len(out.Choices) == 0 {
		return "", nil
	}
	return out.Choices[0].Message.Content, nil
}
```

The `condense` error strings are unchanged (`condense returned HTTP %d`, `condense returned no content`, and so on), so the existing condense tests still hold.

- [ ] **Step 4: Add `Warmup` to `RetrievingResponder`**

In `chat-service/internal/responder/retrieve.go`, add the interface below `condenser`:

```go
type warmer interface {
	warm(ctx context.Context) error
}
```

Add the fields to the struct:

```go
type RetrievingResponder struct {
	Inner     grounder
	Embedder  rag.Embedder
	Store     searcher
	Condenser condenser
	Warm      warmer
	TopK      int
	MinScore  float32
	Floor     int

	cache warmCache
}
```

Add the method below `Stream`:

```go
// Warmup readies both models a turn needs. The probe text is irrelevant;
// embedding anything loads the embedder.
func (r *RetrievingResponder) Warmup(ctx context.Context) error {
	return r.cache.do(ctx, func(ctx context.Context) error {
		if _, err := r.Embedder.Embed(ctx, []string{"warmup"}); err != nil {
			return fmt.Errorf("warming embedder: %w", err)
		}
		if err := r.Warm.warm(ctx); err != nil {
			return fmt.Errorf("warming model: %w", err)
		}
		return nil
	})
}
```

`retrieve.go` already imports `fmt`. Confirm with `go build`.

- [ ] **Step 5: Wire it in `newRetriever`**

In `chat-service/internal/responder/new.go`, add `Warm: inner,` to the returned literal after `Condenser: inner,`:

```go
	return &RetrievingResponder{
		Inner:     inner,
		Embedder:  embedder,
		Store:     store,
		Condenser: inner,
		Warm:      inner,
		TopK:      cfg.TopK,
		MinScore:  float32(cfg.MinScore),
		Floor:     cfg.Floor,
	}, nil
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `cd chat-service && go test ./... && go vet ./...`
Expected: PASS. `vet` must not report `copylocks`: `RetrievingResponder` now holds a mutex and is only ever used by pointer.

- [ ] **Step 7: Commit**

```bash
git add chat-service/internal/responder chat-service/main_test.go
git commit -m "feat(chat): warm the embedder and model through RetrievingResponder"
```

---

### Task 4: `Warmup` handler, metrics and wiring

**Files:**
- Modify: `chat-service/chat.go` (the `chatServer` struct, plus a new method)
- Modify: `chat-service/metrics.go`
- Modify: `chat-service/main.go:54-58`
- Test: `chat-service/chat_test.go`

**Interfaces:**
- Consumes: `responder.Warmer` (Task 2), `responder.ClassifyOutcome`, `chatpb.WarmupRequest` / `WarmupResponse` (Task 1).
- Produces: `chatServer.warmer responder.Warmer` (nil means always warm), `chatServer.Warmup`, and the collectors `chatWarmupsTotal` / `chatWarmupDuration`.

- [ ] **Step 1: Write the failing tests**

Append to `chat-service/chat_test.go`:

```go
type fakeWarmer struct{ err error }

func (w fakeWarmer) Warmup(context.Context) error { return w.err }

// Echo mode has no warmer; the GPU-less stack must still reach the welcome.
func TestWarmupWithoutWarmerSucceeds(t *testing.T) {
	before := counterValue(chatWarmupsTotal.WithLabelValues("ok"))

	if _, err := newTestServer().Warmup(context.Background(), &chatpb.WarmupRequest{}); err != nil {
		t.Fatalf("Warmup() error = %v, want nil", err)
	}
	if got := counterValue(chatWarmupsTotal.WithLabelValues("ok")); got != before+1 {
		t.Errorf("ok count = %v, want %v", got, before+1)
	}
}

// Provider error bodies can carry account detail; the browser gets a generic
// Unavailable and the detail stays in the log.
func TestWarmupHidesProviderDetail(t *testing.T) {
	before := counterValue(chatWarmupsTotal.WithLabelValues("error"))
	srv := &chatServer{
		responder: &responder.EchoResponder{},
		warmer:    fakeWarmer{err: errors.New("warmup returned HTTP 401: invalid key gsk_secret")},
	}

	_, err := srv.Warmup(context.Background(), &chatpb.WarmupRequest{})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", status.Code(err))
	}
	if strings.Contains(err.Error(), "gsk_secret") || strings.Contains(err.Error(), "401") {
		t.Errorf("error %q leaks provider detail", err)
	}
	if got := counterValue(chatWarmupsTotal.WithLabelValues("error")); got != before+1 {
		t.Errorf("error count = %v, want %v", got, before+1)
	}
}

// A tab closed during a cold load is a hangup, not an outage.
func TestWarmupCancelledIsNotAnError(t *testing.T) {
	errBefore := counterValue(chatWarmupsTotal.WithLabelValues("error"))
	cancelledBefore := counterValue(chatWarmupsTotal.WithLabelValues("cancelled"))
	srv := &chatServer{responder: &responder.EchoResponder{}, warmer: fakeWarmer{err: context.Canceled}}

	_, err := srv.Warmup(context.Background(), &chatpb.WarmupRequest{})
	if status.Code(err) != codes.Canceled {
		t.Errorf("code = %v, want Canceled", status.Code(err))
	}
	if got := counterValue(chatWarmupsTotal.WithLabelValues("cancelled")); got != cancelledBefore+1 {
		t.Errorf("cancelled count = %v, want %v", got, cancelledBefore+1)
	}
	if got := counterValue(chatWarmupsTotal.WithLabelValues("error")); got != errBefore {
		t.Errorf("error count = %v, want unchanged %v", got, errBefore)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd chat-service && go test -run TestWarmup .`
Expected: FAIL to compile with `undefined: chatWarmupsTotal` and `unknown field warmer`.

- [ ] **Step 3: Add the collectors**

In `chat-service/metrics.go`, lift the stream buckets into a shared var and add the two collectors. Replace the `chatStreamDuration` entry and add the new ones inside the `var (...)` block:

```go
	chatStreamDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "chat_stream_duration_seconds",
		Help:    "Wall time of a chat stream from request to terminal frame.",
		Buckets: modelLatencyBuckets,
	})

	chatWarmupsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chat_warmups_total",
		Help: "Total Warmup calls by outcome.",
	}, []string{"outcome"})

	chatWarmupDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "chat_warmup_duration_seconds",
		Help:    "Wall time of a Warmup call, including any shared cold load.",
		Buckets: modelLatencyBuckets,
	})
```

Above the `var (` block:

```go
// DefBuckets stop at 10s, which lands every model-backed call in +Inf. The low
// buckets stay for the echo stub, which finishes sooner.
var modelLatencyBuckets = []float64{.1, .25, .5, 1, 2, 5, 10, 30, 60, 120, 300}
```

Delete the old inline comment and bucket literal from `chatStreamDuration`.

- [ ] **Step 4: Add the handler**

In `chat-service/chat.go`, add `"context"` to the imports and the field to the struct:

```go
type chatServer struct {
	chatpb.UnimplementedChatServiceServer
	responder responder.Responder
	warmer    responder.Warmer // nil: nothing to warm, always ready
}
```

Add below `Chat`:

```go
// Warmup records exactly one chatWarmupsTotal increment and one
// chatWarmupDuration observation, from the single deferred func.
func (s *chatServer) Warmup(ctx context.Context, _ *chatpb.WarmupRequest) (*chatpb.WarmupResponse, error) {
	start := time.Now()
	outcome := "ok"
	defer func() {
		chatWarmupsTotal.WithLabelValues(outcome).Inc()
		chatWarmupDuration.Observe(time.Since(start).Seconds())
	}()

	if s.warmer == nil {
		return &chatpb.WarmupResponse{}, nil
	}
	if err := s.warmer.Warmup(ctx); err != nil {
		outcome = responder.ClassifyOutcome(err)
		log := obs.LogWithTrace(ctx, slog.Default())
		if outcome == "cancelled" {
			log.Info("warmup abandoned", "error", err)
			return nil, status.FromContextError(err).Err()
		}
		// Provider error text can carry account detail; it stays in the log.
		log.Warn("warmup failed", "error", err)
		return nil, status.Error(codes.Unavailable, "the assistant is unavailable")
	}
	return &chatpb.WarmupResponse{}, nil
}
```

- [ ] **Step 5: Wire the warmer in `main`**

In `chat-service/main.go`, replace the `RegisterChatServiceServer` line:

```go
	// Echo has no Warmer, so the assertion leaves warmer nil.
	warmer, _ := resp.(responder.Warmer)
	chatpb.RegisterChatServiceServer(grpcServer, &chatServer{responder: resp, warmer: warmer})
```

`main.go` already imports `chat-service/internal/responder` through `newResponder`'s signature. Confirm with `go build`.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `cd chat-service && go test ./... && go vet ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add chat-service/chat.go chat-service/metrics.go chat-service/main.go chat-service/chat_test.go
git commit -m "feat(chat): serve Warmup with one-site outcome metrics"
```

---

### Task 5: `useWarmup` hook and welcome copy

**Files:**
- Create: `frontend/src/components/chat/welcome.ts`
- Create: `frontend/src/components/chat/useWarmup.ts`
- Test: `frontend/src/components/chat/useWarmup.test.ts`

**Interfaces:**
- Consumes: `chatClient.warmup` (Task 1).
- Produces:
  - `export const WELCOME_MESSAGE: string`
  - `export type WarmupStatus = 'warming' | 'ready' | 'unavailable'`
  - `export const WARMUP_TIMEOUT_MS = 130_000`
  - `default export useWarmup(wordDelayMs?: number): { status: WarmupStatus; welcome: string; retry: () => void }`, where `welcome` is the revealed prefix of `WELCOME_MESSAGE`, and `''` until `ready`

- [ ] **Step 1: Write the welcome copy**

`frontend/src/components/chat/welcome.ts`:

```ts
// Page copy, not model output: a small model improvising a greeting could
// state things about Aleksi that are not true.
export const WELCOME_MESSAGE =
  "Hi, welcome to Aleksi Valta's portfolio. I'm an assistant that answers " +
  'questions about his background, experience and this project, which runs ' +
  "on the stack you're talking to right now. Try asking what he has worked " +
  'on, or how this chat is built.';
```

- [ ] **Step 2: Write the failing tests**

`frontend/src/components/chat/useWarmup.test.ts`:

```ts
import { StrictMode } from 'react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { act, renderHook, waitFor } from '@testing-library/react';
import useWarmup from './useWarmup';
import { WELCOME_MESSAGE } from './welcome';
import { WarmupResponse } from '../../gen/chat_pb';
import { chatClient } from '../../api';

vi.mock('../../api', () => ({
  chatClient: { warmup: vi.fn() },
}));

const mockWarmup = vi.mocked(chatClient.warmup);

describe('useWarmup', () => {
  beforeEach(() => {
    mockWarmup.mockReset();
  });

  it('reveals the welcome once the assistant is warm', async () => {
    mockWarmup.mockResolvedValue(new WarmupResponse());

    const { result } = renderHook(() => useWarmup(0));

    expect(result.current.status).toBe('warming');
    expect(result.current.welcome).toBe('');
    await waitFor(() => expect(result.current.welcome).toBe(WELCOME_MESSAGE));
    expect(result.current.status).toBe('ready');
  });

  it('reports unavailable without a welcome when warmup fails', async () => {
    mockWarmup.mockRejectedValue(new Error('the assistant is unavailable'));

    const { result } = renderHook(() => useWarmup(0));

    await waitFor(() => expect(result.current.status).toBe('unavailable'));
    expect(result.current.welcome).toBe('');
  });

  it('warms again on retry', async () => {
    mockWarmup.mockRejectedValueOnce(new Error('the assistant is unavailable'));
    mockWarmup.mockResolvedValue(new WarmupResponse());

    const { result } = renderHook(() => useWarmup(0));
    await waitFor(() => expect(result.current.status).toBe('unavailable'));

    act(() => result.current.retry());

    expect(result.current.status).toBe('warming');
    await waitFor(() => expect(result.current.status).toBe('ready'));
    expect(mockWarmup).toHaveBeenCalledTimes(2);
  });

  // StrictMode mounts, aborts and remounts. A real transport rejects the
  // aborted call late; if that flips status, every dev load shows "offline".
  it('ignores a late rejection from the aborted first mount', async () => {
    mockWarmup.mockImplementationOnce(
      (_req, opts) =>
        new Promise<WarmupResponse>((_, reject) => {
          opts?.signal?.addEventListener('abort', () =>
            setTimeout(() => reject(new Error('aborted')), 20)
          );
        })
    );
    mockWarmup.mockResolvedValue(new WarmupResponse());

    const { result } = renderHook(() => useWarmup(0), { wrapper: StrictMode });

    await waitFor(() => expect(result.current.status).toBe('ready'));
    await new Promise((resolve) => setTimeout(resolve, 40));
    expect(result.current.status).toBe('ready');
  });
});
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `cd frontend && npx vitest run src/components/chat/useWarmup.test.ts`
Expected: FAIL with `Failed to resolve import "./useWarmup"`.

- [ ] **Step 4: Implement**

`frontend/src/components/chat/useWarmup.ts`:

```ts
import { useCallback, useEffect, useState } from 'react';
import { chatClient } from '../../api';
import { WELCOME_MESSAGE } from './welcome';

export type WarmupStatus = 'warming' | 'ready' | 'unavailable';

// Above the server's 120s warm timeout, so a slow cold load fails there with a
// logged reason rather than as a bare client deadline.
export const WARMUP_TIMEOUT_MS = 130_000;

// Paces the welcome like a streamed reply.
const WORD_DELAY_MS = 40;

function useWarmup(wordDelayMs = WORD_DELAY_MS) {
  const [status, setStatus] = useState<WarmupStatus>('warming');
  const [attempt, setAttempt] = useState(0);
  const [welcome, setWelcome] = useState('');

  useEffect(() => {
    const ac = new AbortController();
    chatClient
      .warmup({}, { signal: ac.signal, timeoutMs: WARMUP_TIMEOUT_MS })
      .then(() => setStatus('ready'))
      .catch(() => {
        // StrictMode's discarded mount aborts its call; only the live one reports.
        if (!ac.signal.aborted) setStatus('unavailable');
      });
    return () => ac.abort();
  }, [attempt]);

  useEffect(() => {
    if (status !== 'ready') return;
    const words = WELCOME_MESSAGE.split(' ');
    let shown = 0;
    const id = setInterval(() => {
      shown += 1;
      setWelcome(words.slice(0, shown).join(' '));
      if (shown >= words.length) clearInterval(id);
    }, wordDelayMs);
    return () => clearInterval(id);
  }, [status, wordDelayMs]);

  const retry = useCallback(() => {
    setStatus('warming');
    setAttempt((n) => n + 1);
  }, []);

  return { status, welcome, retry };
}

export default useWarmup;
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `cd frontend && npx vitest run src/components/chat/useWarmup.test.ts && npm run lint`
Expected: 4 tests PASS and lint is clean. If `react-hooks` flags anything, fix it before moving on rather than disabling the rule.

- [ ] **Step 6: Commit**

```bash
git add frontend/src/components/chat/welcome.ts frontend/src/components/chat/useWarmup.ts frontend/src/components/chat/useWarmup.test.ts
git commit -m "feat(frontend): warm the assistant on mount and reveal a welcome"
```

---

### Task 6: `ChatPanel` warming, welcome and unavailable states

**Files:**
- Modify: `frontend/src/components/chat/ChatPanel.tsx`
- Modify: `frontend/src/components/chat/ChatPanel.css`
- Test: `frontend/src/components/chat/ChatPanel.test.tsx`

**Interfaces:**
- Consumes: `useWarmup`, `WarmupStatus` (Task 5); `useChatStream` (unchanged).

- [ ] **Step 1: Write the failing tests**

`frontend/src/components/chat/ChatPanel.test.tsx`:

```tsx
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import ChatPanel from './ChatPanel';
import { ChatChunk, Role, WarmupResponse } from '../../gen/chat_pb';
import { chatClient } from '../../api';

vi.mock('../../api', () => ({
  chatClient: { chat: vi.fn(), warmup: vi.fn() },
}));

const mockChat = vi.mocked(chatClient.chat);
const mockWarmup = vi.mocked(chatClient.warmup);

// No jest-dom in this repo; plain DOM properties instead of its matchers.
function textbox(): HTMLInputElement {
  return screen.getByRole('textbox') as HTMLInputElement;
}

describe('ChatPanel', () => {
  beforeEach(() => {
    mockChat.mockReset();
    mockWarmup.mockReset();
  });

  // A message typed during a cold load would pay the load anyway and race
  // the welcome into the list.
  it('keeps the input disabled until the assistant is warm', async () => {
    let release!: () => void;
    mockWarmup.mockReturnValue(
      new Promise<WarmupResponse>((resolve) => {
        release = () => resolve(new WarmupResponse());
      })
    );

    render(<ChatPanel />);

    expect(textbox().disabled).toBe(true);
    expect(screen.getByText('Assistant is warming up…')).toBeTruthy();

    release();

    await waitFor(() => expect(textbox().disabled).toBe(false));
    expect(await screen.findByText(/^Hi,/)).toBeTruthy();
  });

  // The welcome is page copy. If it leaked into history the model would
  // treat it as its own prior turn.
  it('sends only the user turn after the welcome', async () => {
    mockWarmup.mockResolvedValue(new WarmupResponse());
    mockChat.mockImplementation(() =>
      (async function* () {
        yield new ChatChunk({ event: { case: 'textDelta', value: 'hello' } });
      })()
    );

    render(<ChatPanel />);
    await screen.findByText(/^Hi,/);

    fireEvent.change(textbox(), { target: { value: 'who is he?' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));

    await waitFor(() => expect(mockChat).toHaveBeenCalledTimes(1));
    expect(mockChat.mock.calls[0][0].messages).toEqual([
      { role: Role.USER, content: 'who is he?' },
    ]);
  });

  it('offers a retry when the assistant is unavailable', async () => {
    mockWarmup.mockRejectedValueOnce(new Error('the assistant is unavailable'));
    mockWarmup.mockResolvedValue(new WarmupResponse());

    render(<ChatPanel />);

    fireEvent.click(await screen.findByRole('button', { name: 'Retry' }));

    await waitFor(() => expect(textbox().disabled).toBe(false));
    expect(mockWarmup).toHaveBeenCalledTimes(2);
  });
});
```


- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd frontend && npx vitest run src/components/chat/ChatPanel.test.tsx`
Expected: FAIL. The input is not disabled, and there is no "Assistant is warming up…" text.

- [ ] **Step 3: Implement the panel changes**

In `frontend/src/components/chat/ChatPanel.tsx`:

Add the imports:

```tsx
import useWarmup, { type WarmupStatus } from './useWarmup';
```

Below `PIN_SLACK_PX`:

```tsx
const PLACEHOLDER: Record<WarmupStatus, string> = {
  warming: 'Assistant is warming up…',
  ready: 'Say something',
  unavailable: 'Assistant is offline',
};
```

In the component, after the `useChatStream()` line:

```tsx
  const { status, welcome, retry } = useWarmup();
```

Change the follow-scroll effect's dependencies so the revealing welcome also pins:

```tsx
  }, [messages, welcome]);
```

Replace the `<ol>…</ol>` and the error line with:

```tsx
      <ol className="chat-messages" ref={listRef} onScroll={handleScroll}>
        {status === 'warming' && (
          <li className="chat-status">{PLACEHOLDER.warming}</li>
        )}
        {welcome && <li className="chat-assistant">{welcome}</li>}
        {messages.map((m, i) => (
          <li
            key={i}
            className={m.role === Role.USER ? 'chat-user' : 'chat-assistant'}
          >
            {m.content}
            {streaming && i === messages.length - 1 && (
              <span className="chat-cursor">▌</span>
            )}
          </li>
        ))}
      </ol>

      {status === 'unavailable' && (
        <p className="chat-error">
          The assistant is offline right now.{' '}
          <button type="button" onClick={retry}>
            Retry
          </button>
        </p>
      )}
      {error && <p className="chat-error">{error}</p>}
```

On the `<input>`, replace `placeholder="Say something"` with:

```tsx
          placeholder={PLACEHOLDER[status]}
          disabled={status !== 'ready'}
```

Change the Send button's `disabled`:

```tsx
          <button type="submit" disabled={status !== 'ready' || !input.trim()}>
```

The existing comment above `<input>` ("Deliberately not disabled while streaming…") still holds: the field is disabled only before `ready`, never during a reply.

- [ ] **Step 4: Style the status line**

In `frontend/src/components/chat/ChatPanel.css`, inside `.chat-messages { … }` after `.chat-assistant { … }`:

```css
  .chat-status {
    align-self: flex-start;
    color: var(--text);
    opacity: 0.7;
    font-style: italic;
  }
```

- [ ] **Step 5: Run everything**

Run: `cd frontend && npm test && npm run lint && npm run build`
Expected: all vitest files pass (the existing `useChatStream` tests are untouched), lint is clean and the build succeeds.

- [ ] **Step 6: Commit**

```bash
git add frontend/src/components/chat/ChatPanel.tsx frontend/src/components/chat/ChatPanel.css frontend/src/components/chat/ChatPanel.test.tsx
git commit -m "feat(frontend): show warming, welcome and offline states in the chat"
```

---

### Task 7: End-to-end check and docs

**Files:**
- Modify: `CLAUDE.md` (the **Chat streaming** paragraph)
- Modify: `docs/roadmap.md` (step 4's Shipped / Remaining lists)

- [ ] **Step 1: Echo-mode smoke test**

With `RESPONDER=echo` in `chat-service/.env`:

```bash
docker compose up --build -d
cd frontend && npm run dev
```

In the browser, the welcome should appear within about a second and the input should become enabled. Then check the metrics:

```bash
docker compose exec prometheus wget -qO- 'http://localhost:9090/api/v1/query?query=chat_warmups_total'
```

Expected: a series with `outcome="ok"`.

- [ ] **Step 2: Unavailable smoke test**

```bash
docker compose stop chat-service
```

Reload the page. Expected: "The assistant is offline right now." with a Retry button. Then run `docker compose start chat-service`, click Retry, and the welcome should appear.

- [ ] **Step 3: llm-mode smoke test (needs the GPU)**

With `RESPONDER=llm` and a built index:

```bash
docker compose restart ollama
docker compose --profile llm up -d
```

Ollama restarting unloads the model. Reload the page. Expected: "Assistant is warming up…" for up to about 30s, then the welcome. The first question should then stream its first token without a cold-load pause. Reload again: the welcome should be near-instant (served from the cache). Check `docker compose logs chat-service` for no `warmup failed` lines.

- [ ] **Step 4: Update CLAUDE.md**

In the **Chat streaming** paragraph, after the sentence ending "…since the endpoint is unauthenticated).", insert:

```markdown
A unary `Warmup` RPC, called on page load, readies the provider before the first turn; `RetrievingResponder` shares one in-flight warmup and reuses a success for 20 minutes (`internal/responder/warmup.go`), and echo has nothing to warm.
```

- [ ] **Step 5: Update the roadmap**

In `docs/roadmap.md`, step 4: remove the warmup bullet (the two lines starting `- Warm the model on page load…`) from **Remaining:**, and add to the end of **Shipped:**:

```markdown
- `Warmup` RPC on page load: loads both models on Ollama, verifies the
  provider on a hosted one, then shows a fixed welcome message
```

- [ ] **Step 6: Commit**

```bash
git add CLAUDE.md docs/roadmap.md
git commit -m "docs: record chat warmup in CLAUDE.md and the roadmap"
```
