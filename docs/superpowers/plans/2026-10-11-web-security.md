# Web Security Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Cap what one visitor and the whole site can spend per UTC day, stop floods and oversized or cross-site requests at Envoy, cap concurrent streams, and alert on spend through Alertmanager.

**Architecture:** Envoy resolves the client address, writes it to `x-client-ip`, rate-limits per address and rejects large bodies and foreign origins. chat-service keys an in-memory ledger (`internal/budget`) on that address: it acquires a stream slot and checks the budget before the turn, and debits the provider-reported usage after. Refusals are `ResourceExhausted` with a `google.rpc.ErrorInfo` reason the frontend maps to fixed copy. Prometheus rules feed a new Alertmanager.

**Tech Stack:** Go 1.26, grpc-go, `google.golang.org/genproto/googleapis/rpc/errdetails`, Prometheus client_golang; Envoy v1.39.3 (`local_ratelimit` wildcard descriptors, `buffer`); React 19, Connect-ES v1, `@bufbuild/protobuf` v1, Vitest; Prometheus rules + `promtool`, Alertmanager v0.28.1.

**Spec:** `docs/superpowers/specs/2026-10-11-web-security-design.md`

## Deviations from the spec, decided while planning

- **Client address header is `x-client-ip`, not `x-envoy-external-address`.** Envoy only sets the latter for requests it considers external, and Docker's bridge addresses are private, so it would be missing locally. `request_headers_to_add` with `%DOWNSTREAM_REMOTE_ADDRESS_WITHOUT_PORT%` and `OVERWRITE_IF_EXISTS_OR_ADD` gives the same trust property (client values are overwritten) on every request.
- **`Balance.resets_at` is an RFC 3339 `string`, not `google.protobuf.Timestamp`.** It matches the `resets_at` in `ErrorInfo` metadata and avoids a WKT import on both codegen toolchains.
- **A guard-refused turn is charged for a condense call that already completed.** Condense runs in parallel with the guard; leaving its tokens uncharged lets a flood of flagged messages with long histories spend input tokens outside the budget. A condense cancelled mid-flight reports nothing; the Envoy rate limit bounds that.
- **Embedding tokens are counted, not charged.** The spec leaves them out because they run locally; a hosted embedder bills them. They cost a fraction of chat tokens, so charging them at the same rate as chat tokens would drain budgets for no real spend. `chat_embed_tokens_total` keeps them visible, and the budget gate already stops an exhausted visitor before any embed.
- **Echo reports its word count as output tokens** (existing behaviour), so the budget is exercisable locally without a model.
- **Refusal copy shows in the existing error banner**, not as an assistant bubble.
- **The `/` catch-all route is removed; the `order_service_cluster` definition stays**, so restoring the order path is re-adding one route.

## Global Constraints

- Every limit is an env var; zero, negative, missing or unparsable is a startup error naming the variable, never "unlimited".
- `BUDGET_TOKENS_PER_VISITOR=20000`, `BUDGET_TOKENS_GLOBAL=500000`, `BUDGET_MAX_VISITORS=100000`, `MAX_OUTPUT_TOKENS=800`, `MAX_STREAMS_GLOBAL=16`, `MAX_STREAMS_PER_VISITOR=2`.
- Refusal reasons are wire contract, exactly: `TOO_MANY_STREAMS`, `BUDGET_EXHAUSTED`, `GLOBAL_BUDGET_EXHAUSTED`; `ErrorInfo.Domain` is `chat.v1`; metadata key `resets_at` (RFC 3339, UTC).
- Days are UTC; the ledger resets lazily at UTC midnight.
- IPv6 visitors key on their /64; IPv4-mapped IPv6 keys as plain IPv4.
- No visitor address in any metric label or log line.
- Envoy: image `envoyproxy/envoy:v1.39.3`; body limit 65536 bytes; per-IP bucket 10 tokens, refilled 1 per 2s (30/min, burst 10); allowed origin `http://localhost:5173`.
- Alert threshold `350000` = 70% of `BUDGET_TOKENS_GLOBAL`; a NOTE marks both places.
- `chatStreamsTotal` is still incremented exactly once per call, from the existing deferred func.
- Comments follow CLAUDE.md: the non-obvious why, three lines max.
- `.env` files are off-limits to agents; edit only `.env.example`. The human updates their own `services/chat-service/.env`.

## Review Focus

1. **A browser hanging up mid-stream must free its stream slot.** Otherwise two closed tabs lock a visitor out until restart. → Task 5 test `TestChatReleasesTheSlotWhenTheClientHangsUp`.
2. **A provider failing mid-stream still costs tokens.** The handler must debit partial usage on the error path, or a failing provider is a free ride. → Task 5 test `TestChatDebitsPartialUsageOnError`.
3. **A request that reached chat-service without Envoy's header** (misconfigured proxy, direct access) must be refused, not pooled into one shared bucket that one visitor can exhaust for everyone. → Task 5 test `TestChatRefusesWithoutClientAddress`.
4. **Envoy on a dual-stack listener reports `::ffff:203.0.113.7`.** That must key the same as `203.0.113.7`, or one visitor gets two budgets. → Task 1 test case in `TestVisitorKey`.
5. **Midnight passing during an open stream.** The day's reset must not reset open-stream counts (or the cap is bypassed at midnight), and the debit lands on the new day. → Task 1 test `TestMidnightResetKeepsOpenStreams`.

---

### Task 1: Ledger package `internal/budget`

**Files:**
- Create: `services/chat-service/internal/budget/budget.go`
- Create: `services/chat-service/internal/budget/metrics.go`
- Test: `services/chat-service/internal/budget/budget_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Reason string`; consts `TooManyStreams`, `Exhausted`, `GlobalExhausted`
  - `type RefusedError struct { Reason Reason; ResetsAt time.Time }` (implements `error`; `ResetsAt` zero for `TooManyStreams`)
  - `type Config struct { TokensPerVisitor, TokensGlobal int64; MaxVisitors, MaxStreamsGlobal, MaxStreamsPerVisitor int }`
  - `type Balance struct { Used, Remaining int64; ResetsAt time.Time }`
  - `func New(cfg Config, now func() time.Time) *Ledger` (nil `now` = `time.Now`)
  - `func (l *Ledger) Acquire(key string) (release func(), err error)`; err is `*RefusedError`
  - `func (l *Ledger) Debit(key string, tokens int64) Balance`
  - `func (l *Ledger) Balance(key string) Balance`
  - `func VisitorKey(addr string) (string, error)`

- [ ] **Step 1: Write the failing tests**

`services/chat-service/internal/budget/budget_test.go`:

```go
package budget

import (
	"errors"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

var noon = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func testConfig() Config {
	return Config{
		TokensPerVisitor:     1000,
		TokensGlobal:         5000,
		MaxVisitors:          10,
		MaxStreamsGlobal:     4,
		MaxStreamsPerVisitor: 2,
	}
}

func newTestLedger(cfg Config) (*Ledger, *fakeClock) {
	c := &fakeClock{t: noon}
	return New(cfg, c.Now), c
}

func refusal(t *testing.T, err error) *RefusedError {
	t.Helper()
	var r *RefusedError
	if !errors.As(err, &r) {
		t.Fatalf("err = %v, want *RefusedError", err)
	}
	return r
}

func TestDebitReportsTheBalance(t *testing.T) {
	l, _ := newTestLedger(testConfig())
	release, err := l.Acquire("a")
	if err != nil {
		t.Fatal(err)
	}
	release()

	b := l.Debit("a", 300)
	want := Balance{Used: 300, Remaining: 700, ResetsAt: time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)}
	if b != want {
		t.Errorf("Debit() = %+v, want %+v", b, want)
	}
}

// A visitor at the cap must be refused before the model runs, with the reset
// time the frontend shows.
func TestSpentVisitorIsRefused(t *testing.T) {
	l, _ := newTestLedger(testConfig())
	l.Debit("a", 1000)

	_, err := l.Acquire("a")
	r := refusal(t, err)
	if r.Reason != Exhausted || !r.ResetsAt.Equal(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("refusal = %+v, want BUDGET_EXHAUSTED resetting at next UTC midnight", r)
	}
	if _, err := l.Acquire("b"); err != nil {
		t.Errorf("another visitor was refused: %v", err)
	}
}

// Charged after the turn, so one turn may overshoot; the next one must not start.
func TestOvershootIsBoundedToOneTurn(t *testing.T) {
	l, _ := newTestLedger(testConfig())
	l.Debit("a", 999)
	release, err := l.Acquire("a")
	if err != nil {
		t.Fatalf("a visitor with 1 token left was refused: %v", err)
	}
	release()
	if b := l.Debit("a", 800); b.Remaining != 0 || b.Used != 1799 {
		t.Errorf("balance = %+v, want used 1799, remaining clamped to 0", b)
	}
	if _, err := l.Acquire("a"); refusal(t, err).Reason != Exhausted {
		t.Error("the turn after an overshoot was not refused")
	}
}

// The provider spend cap is global; this is the layer that refuses before it.
func TestSpentSiteRefusesEveryone(t *testing.T) {
	cfg := testConfig()
	cfg.TokensGlobal = 1500
	l, _ := newTestLedger(cfg)
	l.Debit("a", 900)
	l.Debit("b", 600)

	_, err := l.Acquire("c")
	if r := refusal(t, err); r.Reason != GlobalExhausted || r.ResetsAt.IsZero() {
		t.Errorf("refusal = %+v, want GLOBAL_BUDGET_EXHAUSTED with a reset time", r)
	}
}

func TestDailyResetRestoresBudgets(t *testing.T) {
	l, clock := newTestLedger(testConfig())
	l.Debit("a", 1000)
	clock.t = time.Date(2026, 10, 12, 0, 0, 1, 0, time.UTC)

	release, err := l.Acquire("a")
	if err != nil {
		t.Fatalf("refused after midnight UTC: %v", err)
	}
	release()
	if b := l.Balance("a"); b.Used != 0 || b.Remaining != 1000 {
		t.Errorf("balance after reset = %+v, want a full budget", b)
	}
}

// If midnight reset open-stream counts, every visitor could open a fresh pair
// of streams at 00:00 on top of the ones still running.
func TestMidnightResetKeepsOpenStreams(t *testing.T) {
	l, clock := newTestLedger(testConfig())
	r1, _ := l.Acquire("a")
	r2, _ := l.Acquire("a")
	clock.t = time.Date(2026, 10, 12, 0, 0, 1, 0, time.UTC)

	if _, err := l.Acquire("a"); refusal(t, err).Reason != TooManyStreams {
		t.Error("midnight reset freed stream slots that are still open")
	}
	if b := l.Debit("a", 50); b.Used != 50 {
		t.Errorf("a debit straddling midnight = %+v, want it charged to the new day", b)
	}
	r1()
	r2()
}

func TestStreamCaps(t *testing.T) {
	l, _ := newTestLedger(testConfig())
	r1, _ := l.Acquire("a")
	r2, _ := l.Acquire("a")

	_, err := l.Acquire("a")
	if r := refusal(t, err); r.Reason != TooManyStreams || !r.ResetsAt.IsZero() {
		t.Errorf("refusal = %+v, want TOO_MANY_STREAMS without a reset time", r)
	}

	r3, _ := l.Acquire("b")
	r4, _ := l.Acquire("b")
	if _, err := l.Acquire("c"); refusal(t, err).Reason != TooManyStreams {
		t.Error("the site-wide stream cap did not hold")
	}

	r1()
	if _, err := l.Acquire("a"); err != nil {
		t.Errorf("a released slot was not reusable: %v", err)
	}
	r2()
	r3()
	r4()
}

// release is deferred and may also run on an error path; a second call must
// not free someone else's slot.
func TestReleaseIsIdempotent(t *testing.T) {
	l, _ := newTestLedger(testConfig())
	r1, _ := l.Acquire("a")
	r2, _ := l.Acquire("a")
	r1()
	r1()

	if _, err := l.Acquire("a"); err != nil {
		t.Fatalf("slot not freed: %v", err)
	}
	if _, err := l.Acquire("a"); refusal(t, err).Reason != TooManyStreams {
		t.Error("a double release freed two slots")
	}
	r2()
}

// A spray of fresh addresses must not grow memory without bound: past the
// cap, new visitors are refused (fail closed) and known ones still served.
func TestVisitorCapFailsClosed(t *testing.T) {
	cfg := testConfig()
	cfg.MaxVisitors = 2
	l, _ := newTestLedger(cfg)
	for _, k := range []string{"a", "b"} {
		r, err := l.Acquire(k)
		if err != nil {
			t.Fatal(err)
		}
		r()
	}

	if _, err := l.Acquire("c"); refusal(t, err).Reason != GlobalExhausted {
		t.Error("a visitor past the cap was admitted")
	}
	r, err := l.Acquire("a")
	if err != nil {
		t.Errorf("a known visitor was refused at the cap: %v", err)
	}
	r()
}

func TestBalanceDoesNotRegisterAVisitor(t *testing.T) {
	cfg := testConfig()
	cfg.MaxVisitors = 1
	l, _ := newTestLedger(cfg)
	l.Balance("crawler")

	if _, err := l.Acquire("a"); err != nil {
		t.Errorf("a Balance lookup took a visitor slot: %v", err)
	}
}

func TestVisitorKey(t *testing.T) {
	tests := []struct {
		addr, want string
	}{
		{"203.0.113.7", "203.0.113.7"},
		// Dual-stack listeners report IPv4 clients this way.
		{"::ffff:203.0.113.7", "203.0.113.7"},
		// One host owns a whole /64 and could rotate through it.
		{"2001:db8:1:2:aaaa::1", "2001:db8:1:2::/64"},
		{"2001:db8:1:2:bbbb::9", "2001:db8:1:2::/64"},
	}
	for _, tt := range tests {
		got, err := VisitorKey(tt.addr)
		if err != nil || got != tt.want {
			t.Errorf("VisitorKey(%q) = %q, %v; want %q", tt.addr, got, err, tt.want)
		}
	}
	for _, bad := range []string{"", "unknown", "203.0.113.7:443"} {
		if _, err := VisitorKey(bad); err == nil {
			t.Errorf("VisitorKey(%q) succeeded, want an error", bad)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd services/chat-service && go test ./internal/budget/`
Expected: FAIL, build errors such as `undefined: New`.

- [ ] **Step 3: Write the metrics**

`services/chat-service/internal/budget/metrics.go`:

```go
package budget

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// No visitor identity in any label: one series per address would grow without
// bound and put personal data in the metrics store.
var (
	budgetRefusalsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chat_budget_refusals_total",
		Help: "Turns refused before reaching the model, by reason.",
	}, []string{"reason"})

	budgetGlobalUsed = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "chat_budget_global_used_tokens",
		Help: "Tokens charged site-wide since the last UTC midnight or restart.",
	})

	budgetVisitorTokens = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "chat_budget_visitor_tokens",
		Help:    "A visitor's running daily total, observed at each debit. Tune BUDGET_TOKENS_PER_VISITOR from it.",
		Buckets: []float64{500, 1000, 2000, 5000, 10000, 15000, 20000, 30000, 50000},
	})
)
```

- [ ] **Step 4: Write the ledger**

`services/chat-service/internal/budget/budget.go`:

```go
// Package budget caps the tokens each visitor and the whole site may spend per
// UTC day, and the streams they may hold open. State is in memory: a restart
// resets it, and the provider's own spend cap is the backstop.
package budget

import (
	"fmt"
	"net/netip"
	"sync"
	"time"
)

// Reason is wire contract: the frontend picks its copy by it.
type Reason string

const (
	TooManyStreams  Reason = "TOO_MANY_STREAMS"
	Exhausted       Reason = "BUDGET_EXHAUSTED"
	GlobalExhausted Reason = "GLOBAL_BUDGET_EXHAUSTED"
)

// RefusedError is returned by Acquire. ResetsAt is zero for TooManyStreams: a
// slot frees when a stream ends, not at midnight.
type RefusedError struct {
	Reason   Reason
	ResetsAt time.Time
}

func (e *RefusedError) Error() string { return "budget: refused, " + string(e.Reason) }

type Config struct {
	TokensPerVisitor     int64
	TokensGlobal         int64
	MaxVisitors          int
	MaxStreamsGlobal     int
	MaxStreamsPerVisitor int
}

type Balance struct {
	Used, Remaining int64
	ResetsAt        time.Time
}

type Ledger struct {
	cfg Config
	now func() time.Time

	mu     sync.Mutex
	day    time.Time // UTC midnight the token counters belong to
	used   map[string]int64
	global int64
	// Open streams outlive the day boundary, so rollover leaves them alone.
	open      map[string]int
	openTotal int
}

func New(cfg Config, now func() time.Time) *Ledger {
	if now == nil {
		now = time.Now
	}
	return &Ledger{cfg: cfg, now: now, used: map[string]int64{}, open: map[string]int{}}
}

// rollover starts a new day's counters when the UTC date has moved on, and
// returns the next reset. Callers hold mu.
func (l *Ledger) rollover() time.Time {
	t := l.now().UTC()
	today := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	if !today.Equal(l.day) {
		l.day = today
		l.used = map[string]int64{}
		l.global = 0
		budgetGlobalUsed.Set(0)
	}
	return today.AddDate(0, 0, 1)
}

// Acquire takes a stream slot for key if the caps and today's budgets allow.
// The returned release is idempotent; defer it.
func (l *Ledger) Acquire(key string) (release func(), err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	resets := l.rollover()

	if l.openTotal >= l.cfg.MaxStreamsGlobal || l.open[key] >= l.cfg.MaxStreamsPerVisitor {
		return nil, refuse(TooManyStreams, time.Time{})
	}
	used, known := l.used[key]
	switch {
	case l.global >= l.cfg.TokensGlobal:
		return nil, refuse(GlobalExhausted, resets)
	case !known && len(l.used) >= l.cfg.MaxVisitors:
		// Fail closed, like the guard: a spray of fresh addresses must not
		// grow the map without bound.
		return nil, refuse(GlobalExhausted, resets)
	case used >= l.cfg.TokensPerVisitor:
		return nil, refuse(Exhausted, resets)
	}

	l.used[key] = used
	l.open[key]++
	l.openTotal++
	var once sync.Once
	return func() { once.Do(func() { l.release(key) }) }, nil
}

func (l *Ledger) release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.openTotal--
	l.open[key]--
	if l.open[key] <= 0 {
		delete(l.open, key)
	}
}

func refuse(r Reason, resets time.Time) *RefusedError {
	budgetRefusalsTotal.WithLabelValues(string(r)).Inc()
	return &RefusedError{Reason: r, ResetsAt: resets}
}

// Debit charges tokens the provider reported for one turn and returns the
// visitor's balance after it.
func (l *Ledger) Debit(key string, tokens int64) Balance {
	l.mu.Lock()
	defer l.mu.Unlock()
	resets := l.rollover()
	if tokens > 0 {
		l.used[key] += tokens
		l.global += tokens
		budgetGlobalUsed.Set(float64(l.global))
		budgetVisitorTokens.Observe(float64(l.used[key]))
	}
	return l.balance(key, resets)
}

// Balance reads key's balance without registering it as a visitor.
func (l *Ledger) Balance(key string) Balance {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.balance(key, l.rollover())
}

func (l *Ledger) balance(key string, resets time.Time) Balance {
	used := l.used[key]
	return Balance{Used: used, Remaining: max(0, l.cfg.TokensPerVisitor-used), ResetsAt: resets}
}

// VisitorKey maps a client address to the key budgets are kept under. IPv6 is
// truncated to its /64, which one host controls.
func VisitorKey(addr string) (string, error) {
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return "", fmt.Errorf("client address %q: %w", addr, err)
	}
	a = a.Unmap().WithZone("")
	if a.Is4() {
		return a.String(), nil
	}
	p, err := a.Prefix(64)
	if err != nil {
		return "", fmt.Errorf("client address %q: %w", addr, err)
	}
	return p.String(), nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `cd services/chat-service && go test ./internal/budget/ && go vet ./internal/budget/`
Expected: PASS, no vet output.

- [ ] **Step 6: Commit**

```bash
git add services/chat-service/internal/budget
git commit -m "feat(budget): per-visitor and site-wide daily token ledger"
```

---

### Task 2: Per-turn output ceiling and condense usage

**Files:**
- Modify: `services/chat-service/internal/responder/openai.go` (struct `OpenAIResponder` ~L29, `openAIRequest` ~L55, `Stream` ~L106, `condense` ~L290, `warm` ~L306, `complete` ~L313)
- Modify: `services/chat-service/internal/responder/retrieve.go` (`condenser` ~L28, `Stream` ~L53, `ground` ~L166, `query` ~L197)
- Modify: `services/chat-service/internal/responder/responder.go` (`Usage`)
- Modify: `services/chat-service/internal/responder/new.go` (`Config`, `New`)
- Test: `services/chat-service/internal/responder/openai_test.go`, `services/chat-service/internal/responder/retrieve_test.go`

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces:
  - `OpenAIResponder.MaxTokens int`: sent as `max_tokens` on the streaming request; 0 omits it.
  - `responder.Config.MaxOutputTokens int`, passed into `OpenAIResponder.MaxTokens` by `New`.
  - `RetrievingResponder.Stream`'s returned `Usage` includes the condense call's tokens on every path.
  - `func (u Usage) plus(o Usage) Usage` (unexported).
  - `condenser.condense(ctx, msgs) (string, Usage, error)`.

- [ ] **Step 1: Write the failing tests**

In `openai_test.go`, add `MaxTokens int \`json:"max_tokens"\`` to the `captured` struct inside `TestOpenAIResponderRequestShape`, set `r.MaxTokens = 800` after `r := newTestLLM(srv.URL)`, and add after the `IncludeUsage` check:

```go
	// Without it one turn's output is unbounded, and so is its cost.
	if body.MaxTokens != 800 {
		t.Errorf("max_tokens = %d, want 800", body.MaxTokens)
	}
```

Append to `openai_test.go`:

```go
// The budget is charged from Usage; a condense the provider billed but we did
// not count is spend outside every visitor's cap.
func TestOpenAIResponderCondenseReportsUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "standalone?"}}},
			"usage":   map[string]any{"prompt_tokens": 120, "completion_tokens": 7},
		})
	}))
	t.Cleanup(srv.Close)

	o := &OpenAIResponder{BaseURL: srv.URL, Model: "m", Client: srv.Client()}
	_, usage, err := o.condense(context.Background(), []*chatpb.Message{
		{Role: chatpb.Role_ROLE_USER, Content: "a"},
		{Role: chatpb.Role_ROLE_ASSISTANT, Content: "b"},
		{Role: chatpb.Role_ROLE_USER, Content: "c"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if usage.InputTokens != 120 || usage.OutputTokens != 7 {
		t.Errorf("usage = %+v, want 120 in, 7 out", usage)
	}
}
```

In `TestOpenAIResponderCondenseRequestShape`, change `out, err := o.condense(` to `out, _, err := o.condense(`.

In `retrieve_test.go`, replace `fakeCondenser` and its method:

```go
type fakeCondenser struct {
	out   string
	usage Usage
	err   error
	seen  int
}

func (c *fakeCondenser) condense(_ context.Context, _ []*chatpb.Message) (string, Usage, error) {
	c.seen++
	return c.out, c.usage, c.err
}
```

Append to `retrieve_test.go`:

```go
var withHistory = []*chatpb.Message{
	{Role: chatpb.Role_ROLE_USER, Content: "tell me about the chat service"},
	{Role: chatpb.Role_ROLE_ASSISTANT, Content: "It streams over gRPC-Web."},
	{Role: chatpb.Role_ROLE_USER, Content: "what about testing?"},
}

func TestStreamChargesTheCondenseCall(t *testing.T) {
	c := &fakeCondenser{out: "how is it tested?", usage: Usage{InputTokens: 100, OutputTokens: 6}}
	r := newTestRetriever(&fakeSearcher{byKind: map[string][]rag.Hit{}}, &fakeGrounder{}, c)

	usage, err := r.Stream(context.Background(), &chatpb.ChatRequest{Messages: withHistory}, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	// The echo inner reply reports its 3 words as output tokens.
	if usage.InputTokens != 100 || usage.OutputTokens != 9 || usage.StopReason != "end_turn" {
		t.Errorf("usage = %+v, want condense + reply tokens and the reply's stop reason", usage)
	}
}

// Condense runs alongside the guard. A flood of flagged turns with long
// histories would otherwise spend input tokens no budget sees.
func TestRefusedTurnIsChargedForACompletedCondense(t *testing.T) {
	c := &fakeCondenser{out: "q", usage: Usage{InputTokens: 100, OutputTokens: 6}}
	r := newTestRetriever(&fakeSearcher{byKind: map[string][]rag.Hit{}}, &fakeGrounder{}, c)
	r.Inner = &countingGrounder{}
	r.Guard = &slowFlagGuard{after: c}

	usage, err := r.Stream(context.Background(), &chatpb.ChatRequest{Messages: withHistory}, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if usage.StopReason != "guarded" || usage.InputTokens != 100 || usage.OutputTokens != 6 {
		t.Errorf("usage = %+v, want the refusal charged the completed condense", usage)
	}
}

// slowFlagGuard flags every turn, but only once the condenser has run, so the
// test does not depend on goroutine scheduling.
type slowFlagGuard struct{ after *fakeCondenser }

func (g *slowFlagGuard) Check(ctx context.Context, _ []string) (guard.Verdict, error) {
	for g.after.seen.Load() == 0 {
		if err := ctx.Err(); err != nil {
			return guard.Verdict{}, err
		}
		time.Sleep(time.Millisecond)
	}
	return guard.Verdict{Flagged: true, Score: 0.99}, nil
}
```

`slowFlagGuard` reads `seen` without a lock while the grounding goroutine writes it. Make `fakeCondenser.seen` race-safe by changing it to `seen atomic.Int32` (`c.seen.Add(1)` in `condense`; `c.seen.Load()` in the existing assertions `c.seen != 0` → `c.seen.Load() != 0`, `c2.seen != 1` → `c2.seen.Load() != 1`, and in `slowFlagGuard`). Import `sync/atomic` and `time` if not already imported.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd services/chat-service && go test ./internal/responder/`
Expected: FAIL to build: `o.condense` returns 2 values, and `fakeCondenser` does not implement `condenser`.

- [ ] **Step 3: Implement**

`responder.go`, below the `Usage` struct:

```go
// plus adds o's tokens to u, keeping u's StopReason.
func (u Usage) plus(o Usage) Usage {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	return u
}
```

`openai.go`:
- Add field to `OpenAIResponder`: `MaxTokens int // per-turn output ceiling; 0 sends none`.
- Add to `openAIRequest`: `MaxTokens int \`json:"max_tokens,omitempty"\``, and set `MaxTokens: o.MaxTokens` in the `json.Marshal(openAIRequest{...})` call in `Stream`.
- Change `condense` to return usage:

```go
func (o *OpenAIResponder) condense(ctx context.Context, msgs []*chatpb.Message) (string, Usage, error) {
	payload := append([]openAIMessage{{Role: "system", Content: condensePrompt}}, toOpenAIMessages(msgs)...)
	out, usage, err := o.complete(ctx, "condense", condenseMaxTokens, payload)
	if err != nil {
		return "", usage, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", usage, fmt.Errorf("condense returned no content")
	}
	return out, usage, nil
}
```

- `warm`: `_, _, err := o.complete(ctx, "warmup", 1, ...)`.
- `complete` returns `(string, Usage, error)`. Every early `return "", err` becomes `return "", Usage{}, err`. Decode usage alongside choices:

```go
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int32 `json:"prompt_tokens"`
			CompletionTokens int32 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", Usage{}, fmt.Errorf("decoding %s response: %w", op, err)
	}
	var usage Usage
	if out.Usage != nil {
		usage = Usage{InputTokens: out.Usage.PromptTokens, OutputTokens: out.Usage.CompletionTokens}
	}
	if len(out.Choices) == 0 {
		return "", usage, nil
	}
	return out.Choices[0].Message.Content, usage, nil
```

`retrieve.go`:
- `condenser` interface: `condense(ctx context.Context, msgs []*chatpb.Message) (string, Usage, error)`.
- `query` returns `(string, Usage)`:

```go
func (r *RetrievingResponder) query(ctx context.Context, msgs []*chatpb.Message) (string, Usage) {
	raw := lastContent(msgs)
	if len(msgs) < 2 || r.Condenser == nil {
		return raw, Usage{}
	}

	start := time.Now()
	condensed, usage, err := r.Condenser.condense(ctx, msgs)
	chatCondenseDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		// Degraded retrieval beats no answer.
		obs.LogWithTrace(ctx, slog.Default()).Warn("condensation failed, embedding the raw turn", "error", err)
		return raw, usage
	}
	return condensed, usage
}
```

- `ground` returns `(string, Usage, error)`: `query, usage := r.query(ctx, msgs)` at the top, and every return carries `usage` (`return "", usage, retrievalError(...)`, `return corpusEnvelope, usage, nil`, etc.).
- `Stream`:

```go
	type grounded struct {
		system string
		usage  Usage
		err    error
	}
	done := make(chan grounded, 1)
	go func() {
		system, usage, err := r.ground(groundCtx, msgs)
		done <- grounded{system, usage, err}
	}()

	if reply, stop, err := r.screen(ctx, msgs); err != nil || reply != "" {
		cancelGround()
		// A condense that finished before the cancel was billed; charge it.
		condensed := (<-done).usage
		if err != nil {
			return condensed, err
		}
		if err := emit(reply); err != nil {
			return condensed, err
		}
		return Usage{StopReason: stop}.plus(condensed), nil
	}

	g := <-done
	system := g.system
	if g.err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return g.usage, ctxErr
		}
		obs.LogWithTrace(ctx, slog.Default()).Warn("retrieval unavailable, degrading the reply", "error", g.err)
		system = unavailableEnvelope
	}
	usage, err := r.Inner.withSystem(system).Stream(ctx, req, emit)
	return usage.plus(g.usage), err
```

Keep the existing comment above the bare `ctxErr` return.

`new.go`: add `MaxOutputTokens int` to `Config` (next to `Model`), and `MaxTokens: cfg.MaxOutputTokens,` in the `&OpenAIResponder{...}` literal.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd services/chat-service && go test -race ./internal/responder/ && go vet ./...`
Expected: PASS. (If `-race` is unavailable on Windows without cgo, run without it and say so in the report.)

- [ ] **Step 5: Commit**

```bash
git add services/chat-service/internal/responder
git commit -m "feat(responder): cap output per turn and report condense usage"
```

---

### Task 3: Embedding token metric

**Files:**
- Modify: `services/chat-service/internal/rag/embed.go` (package doc ~L1, struct `OpenAIEmbedder` ~L32, `post` ~L91)
- Modify: `services/chat-service/internal/responder/metrics.go`
- Modify: `services/chat-service/internal/responder/new.go` (`newRetriever` ~L80)
- Modify: `services/chat-service/cmd/ingest/main.go`
- Test: `services/chat-service/internal/rag/embed_test.go`

**Interfaces:**
- Consumes: nothing from Tasks 1–2.
- Produces:
  - `OpenAIEmbedder.OnUsage func(tokens int)`: called once per `/embeddings` request with the provider-reported `usage.prompt_tokens`; nil ignores it. Not called when the provider omits `usage`.
  - Counter `chat_embed_tokens_total` (no labels).

Embedding tokens are **counted, not charged**. They cost a fraction of chat tokens, and charging them at the same rate as chat tokens would drain a visitor's budget for almost no spend. The counter keeps the spend visible. The `Embedder` interface is unchanged, and `rag` still exports no metrics: it reports through the callback, and the caller owns the collector.

- [ ] **Step 1: Write the failing test**

Append to `embed_test.go`:

```go
// Hosted embeddings bill per input token. Without this the spend dashboards
// miss every query and every ingest run.
func TestEmbedReportsProviderUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding request: %v", err)
			return
		}
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		out := struct {
			Data  []item `json:"data"`
			Usage struct {
				PromptTokens int `json:"prompt_tokens"`
			} `json:"usage"`
		}{}
		for i := range req.Input {
			out.Data = append(out.Data, item{Embedding: []float32{1, 0}, Index: i})
		}
		out.Usage.PromptTokens = 3 * len(req.Input)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	e, err := NewOpenAIEmbedder(context.Background(), srv.URL, "m", "", srv.Client())
	if err != nil {
		t.Fatalf("NewOpenAIEmbedder() error = %v", err)
	}
	var got int
	e.OnUsage = func(tokens int) { got += tokens }

	texts := make([]string, EmbedBatchSize+8)
	for i := range texts {
		texts[i] = "t"
	}
	if _, err := e.Embed(context.Background(), texts); err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
	if want := 3 * len(texts); got != want {
		t.Errorf("reported %d tokens, want %d summed across both batches", got, want)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd services/chat-service && go test -run TestEmbedReportsProviderUsage ./internal/rag/`
Expected: FAIL to compile, `e.OnUsage undefined`.

- [ ] **Step 3: Implement**

`embed.go` package doc, second and third lines become:

```go
// ingest command. It exports no metrics: the chat service observes durations
// and token usage around these calls, and ingest prints a summary instead.
```

Add to `OpenAIEmbedder`, after `Client`:

```go
	// OnUsage receives each request's provider-reported input tokens; nil
	// ignores them. Called concurrently when Embed is.
	OnUsage func(tokens int)
```

In `post`, decode `usage` alongside `data`, and report it right after a successful decode. The provider has billed the request by then, whatever the vector checks below decide:

```go
	var out struct {
		Data  []embedItem `json:"data"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding embeddings response: %w", err)
	}
	if e.OnUsage != nil && out.Usage.PromptTokens > 0 {
		e.OnUsage(out.Usage.PromptTokens)
	}
```

`metrics.go`: add to the `var` block, after `chatEmbedDuration`:

```go
	chatEmbedTokensTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "chat_embed_tokens_total",
		Help: "Input tokens the embedding provider reported for query embeds. Counted, not charged to any budget; zero if the provider omits usage.",
	})
```

`new.go`, in `newRetriever` right after the `NewOpenAIEmbedder` error check:

```go
	embedder.OnUsage = func(tokens int) { chatEmbedTokensTotal.Add(float64(tokens)) }
```

`cmd/ingest/main.go`: after the `NewOpenAIEmbedder` error check, `var embedTokens int` and `embedder.OnUsage = func(tokens int) { embedTokens += tokens }` (ingest embeds sequentially). After the `embedded` summary line:

```go
	fmt.Printf("  tokens   %4d embedding input tokens reported\n", embedTokens)
```

The dimension probe in the constructor runs before `OnUsage` is set and goes uncounted: two tokens per process start.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd services/chat-service && go test ./... && go vet ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add services/chat-service/internal/rag services/chat-service/internal/responder services/chat-service/cmd/ingest
git commit -m "feat(rag): count provider-reported embedding tokens"
```

---

### Task 4: Proto `Balance` and regenerated code

**Files:**
- Modify: `proto/chat.proto`
- Regenerate: `services/chat-service/chat/chat.pb.go`, `services/chat-service/chat/chat_grpc.pb.go`, `frontend/src/gen/chat_pb.ts`, `frontend/src/gen/chat_connect.ts`

**Interfaces:**
- Produces: Go `chatpb.Balance{TokensUsed int32, TokensRemaining int32, ResetsAt string}`, `chatpb.Done.Balance *chatpb.Balance`, `chatpb.WarmupResponse.Balance *chatpb.Balance`; TS `Balance` class with `tokensUsed`, `tokensRemaining`, `resetsAt`, and `Done.balance?`, `WarmupResponse.balance?`.

- [ ] **Step 1: Edit the proto**

In `proto/chat.proto`, add `Balance balance = 5;` to `Done`, replace `WarmupResponse`, and add `Balance`:

```proto
message Done {
  string stop_reason = 1;
  int32 input_tokens = 2;
  int32 output_tokens = 3;
  bytes signature = 4;  // HMAC over this reply and the question it answered
  Balance balance = 5;  // after this turn was charged
}

// Empty for now; room for e.g. a model id without a new RPC.
message WarmupRequest {}

message WarmupResponse {
  Balance balance = 1;  // unset when the visitor's address is unknown
}

// A visitor's token budget for the current UTC day.
message Balance {
  int32 tokens_used = 1;
  int32 tokens_remaining = 2;
  string resets_at = 3;  // RFC 3339, UTC
}
```

- [ ] **Step 2: Regenerate both sides**

Run from the repo root:

```bash
protoc -I proto --go_out=services/chat-service --go-grpc_out=services/chat-service proto/chat.proto
cd frontend && npx buf generate ../proto && cd ..
```

Expected: `git status` shows the four generated files modified. `git diff --stat frontend/src/gen services/chat-service/chat` shows no file outside those.

- [ ] **Step 3: Verify both sides build**

Run: `cd services/chat-service && go build ./... && cd ../../frontend && npx tsc -b`
Expected: no errors.

- [ ] **Step 4: Commit**

```bash
git add proto/chat.proto services/chat-service/chat frontend/src/gen
git commit -m "feat(proto): token balance on Done and WarmupResponse"
```

---

### Task 5: Handler enforces and reports the budget

**Files:**
- Create: `services/chat-service/internal/server/budget.go`
- Modify: `services/chat-service/internal/server/chat.go`
- Modify: `services/chat-service/cmd/chat-service/main.go` (only the `server.New` call: pass `nil` for now; Task 6 wires the real ledger)
- Test: `services/chat-service/internal/server/chat_test.go`, new `services/chat-service/internal/server/budget_test.go`

**Interfaces:**
- Consumes: Task 1 `budget.Ledger`, `budget.VisitorKey`, `*budget.RefusedError`, `budget.Balance`; Task 4 `chatpb.Balance`.
- Produces: `func New(r responder.Responder, signer *history.Signer, ledger *budget.Ledger) chatpb.ChatServiceServer` (ledger must be non-nil at runtime; Task 6 builds it). Metadata key `x-client-ip` is what Envoy (Task 7) must set.

- [ ] **Step 1: Write the failing tests**

In `chat_test.go`, update the helpers:

```go
// testClientIP stands in for the address Envoy writes into x-client-ip.
const testClientIP = "203.0.113.7"

func newFakeStream(ctx context.Context) *fakeStream {
	return &fakeStream{ctx: metadata.NewIncomingContext(ctx, metadata.Pairs("x-client-ip", testClientIP))}
}

func testLedger() *budget.Ledger {
	return budget.New(budget.Config{
		TokensPerVisitor:     1000,
		TokensGlobal:         100000,
		MaxVisitors:          100,
		MaxStreamsGlobal:     10,
		MaxStreamsPerVisitor: 2,
	}, nil)
}

func newTestServer() *chatServer {
	return &chatServer{signer: testSigner, responder: &responder.EchoResponder{}, ledger: testLedger()}
}
```

Import `google.golang.org/grpc/metadata` and `chat-service/internal/budget`. Every other `&chatServer{...}` literal in `chat_test.go` gains `ledger: testLedger()`; find them with `grep -n "&chatServer{" internal/server/chat_test.go`.

Create `services/chat-service/internal/server/budget_test.go`:

```go
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

// usageResponder reports fixed usage, optionally failing after it.
type usageResponder struct {
	usage responder.Usage
	err   error
}

func (r *usageResponder) Stream(_ context.Context, _ *chatpb.ChatRequest, emit func(string) error) (responder.Usage, error) {
	if r.err == nil {
		if err := emit("ok"); err != nil {
			return r.usage, err
		}
	}
	return r.usage, r.err
}

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
```

Reason strings are asserted as literals, not `budget` constants, on purpose: they are wire contract with the frontend, and a renamed constant must fail these tests.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd services/chat-service && go test ./internal/server/`
Expected: FAIL to build: `chatServer` has no field `ledger`.

- [ ] **Step 3: Write the refusal mapping**

`services/chat-service/internal/server/budget.go`:

```go
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
```

Run `cd services/chat-service && go get google.golang.org/genproto/googleapis/rpc@v0.0.0-20260526163538-3dc84a4a5aaa && go mod tidy` so the already-indirect module becomes a direct dependency at the version `go.mod` already pins.

- [ ] **Step 4: Wire it into the handler**

`chat.go`:
- `chatServer` gains `ledger *budget.Ledger`; `New` becomes `func New(r responder.Responder, signer *history.Signer, ledger *budget.Ledger) chatpb.ChatServiceServer` and sets it.
- In `Chat`, directly after the `validateHistory` block:

```go
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
```

The log line must not include `key` or the header value (no addresses in logs).
- Directly after `recordTokens(usage)`:

```go
	// Charged on every path: a failed turn still consumed what it reports.
	balance := s.ledger.Debit(key, int64(usage.InputTokens)+int64(usage.OutputTokens))
```

- In the `Done` literal add `Balance: toProtoBalance(balance),`.
- In `Warmup`, build the response once and attach the balance on success:

```go
	resp := &chatpb.WarmupResponse{}
	if key, err := visitorKey(ctx); err == nil {
		resp.Balance = toProtoBalance(s.ledger.Balance(key))
	}
```

Return `resp` from both success returns (the `s.warmer == nil` one and the end of the function).
- Imports: add `errors` and `chat-service/internal/budget`.
- Update the doc comment on `Chat` only if it no longer holds; the single deferred accounting is unchanged.

`cmd/chat-service/main.go`: change to `server.New(resp, signer, nil)` so the module builds; Task 6 replaces `nil`.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `cd services/chat-service && go test ./... && go vet ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add services/chat-service
git commit -m "feat(chat): enforce token budgets and stream caps per visitor"
```

---

### Task 6: Configuration and wiring

**Files:**
- Modify: `services/chat-service/cmd/chat-service/config.go`
- Modify: `services/chat-service/cmd/chat-service/main.go`
- Modify: `services/chat-service/.env.example`
- Test: `services/chat-service/cmd/chat-service/main_test.go`

**Interfaces:**
- Consumes: Task 1 `budget.Config`, `budget.New`; Task 2 `responder.Config.MaxOutputTokens`; Task 5 `server.New(r, signer, ledger)`.
- Produces: `func loadBudget() (budget.Config, error)`, `func requirePositive(name string) (int64, error)`.

- [ ] **Step 1: Write the failing tests**

Append to `main_test.go`:

```go
func setBudgetEnv(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"BUDGET_TOKENS_PER_VISITOR": "20000",
		"BUDGET_TOKENS_GLOBAL":      "500000",
		"BUDGET_MAX_VISITORS":       "100000",
		"MAX_STREAMS_GLOBAL":        "16",
		"MAX_STREAMS_PER_VISITOR":   "2",
	} {
		t.Setenv(k, v)
	}
}

func TestLoadBudget(t *testing.T) {
	setBudgetEnv(t)
	cfg, err := loadBudget()
	if err != nil {
		t.Fatal(err)
	}
	want := budget.Config{TokensPerVisitor: 20000, TokensGlobal: 500000, MaxVisitors: 100000, MaxStreamsGlobal: 16, MaxStreamsPerVisitor: 2}
	if cfg != want {
		t.Errorf("loadBudget() = %+v, want %+v", cfg, want)
	}
}

// A blank line in .env must never become an unlimited budget.
func TestLoadBudgetRejectsMissingAndNonPositive(t *testing.T) {
	for _, name := range []string{"BUDGET_TOKENS_PER_VISITOR", "BUDGET_TOKENS_GLOBAL", "BUDGET_MAX_VISITORS", "MAX_STREAMS_GLOBAL", "MAX_STREAMS_PER_VISITOR"} {
		for _, bad := range []string{"", "0", "-5", "lots"} {
			setBudgetEnv(t)
			t.Setenv(name, bad)
			_, err := loadBudget()
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s=%q: err = %v, want an error naming the variable", name, bad, err)
			}
		}
	}
}

func TestMaxOutputTokensIsRequiredForLLM(t *testing.T) {
	t.Setenv("RESPONDER", "llm")
	t.Setenv("MAX_OUTPUT_TOKENS", "0")
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "MAX_OUTPUT_TOKENS") {
		t.Errorf("err = %v, want an error naming MAX_OUTPUT_TOKENS", err)
	}
}
```

Import `chat-service/internal/budget` in the test file.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd services/chat-service && go test ./cmd/chat-service/`
Expected: FAIL to build: `undefined: loadBudget`.

- [ ] **Step 3: Implement**

`config.go`, append:

```go
// requirePositive has no fallback: every limit it reads guards spend, and a
// default would hide a missing line in .env behind numbers nobody chose.
func requirePositive(name string) (int64, error) {
	raw := os.Getenv(name)
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s=%q must be a positive integer; there is no unlimited", name, raw)
	}
	return n, nil
}

func loadBudget() (budget.Config, error) {
	var vals [5]int64
	for i, name := range []string{"BUDGET_TOKENS_PER_VISITOR", "BUDGET_TOKENS_GLOBAL", "BUDGET_MAX_VISITORS", "MAX_STREAMS_GLOBAL", "MAX_STREAMS_PER_VISITOR"} {
		n, err := requirePositive(name)
		if err != nil {
			return budget.Config{}, err
		}
		vals[i] = n
	}
	return budget.Config{
		TokensPerVisitor:     vals[0],
		TokensGlobal:         vals[1],
		MaxVisitors:          int(vals[2]),
		MaxStreamsGlobal:     int(vals[3]),
		MaxStreamsPerVisitor: int(vals[4]),
	}, nil
}
```

In `loadConfig`, as the last check before `return cfg, nil` (llm branch only):

```go
	maxOut, err := requirePositive("MAX_OUTPUT_TOKENS")
	if err != nil {
		return responder.Config{}, err
	}
	cfg.MaxOutputTokens = int(maxOut)
```

Import `chat-service/internal/budget`.

`main.go`, after building `signer`:

```go
	budgetCfg, err := loadBudget()
	if err != nil {
		slog.Error("invalid budget configuration", "error", err)
		os.Exit(1)
	}
	ledger := budget.New(budgetCfg, nil)
```

and register with `server.New(resp, signer, ledger)`. Import `chat-service/internal/budget`.

Existing tests that build a working llm responder now fail on `MAX_OUTPUT_TOKENS`. Run `go test ./cmd/chat-service/`; for each test that fails with an error mentioning `MAX_OUTPUT_TOKENS`, add `t.Setenv("MAX_OUTPUT_TOKENS", "800")` next to its other `t.Setenv` calls (for the table test `TestNewResponder`, add it once before the loop applying `tt.env`, at ~L105).

`.env.example`, append:

```
# Abuse and cost limits. Each is required: a missing, zero or unparsable value
# is a startup error, never "unlimited". Days are UTC; a restart resets budgets.
# Tokens a visitor may spend per day, condense and reply, input and output.
BUDGET_TOKENS_PER_VISITOR=20000
# Site-wide tokens per day. Keep it below the provider's spend cap.
# NOTE: infra/prometheus/rules/chat.yml alerts at 70% of this. Change both together.
BUDGET_TOKENS_GLOBAL=500000
# Distinct visitors per day; past it new visitors are refused, bounding memory.
BUDGET_MAX_VISITORS=100000
# Output ceiling per reply. Bounds how far one turn can overshoot a budget.
MAX_OUTPUT_TOKENS=800
# Concurrent streams. Over the cap is refused at once, never queued.
MAX_STREAMS_GLOBAL=16
MAX_STREAMS_PER_VISITOR=2
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd services/chat-service && go test ./... && go vet ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add services/chat-service
git commit -m "feat(chat): load budget limits from the environment"
```

---

### Task 7: Envoy edge hardening

**Files:**
- Modify: `infra/envoy/envoy.yaml` (full replacement below)
- Modify: `docker-compose.yml` (envoy image)
- Create: `infra/envoy/check-edge.sh`

**Interfaces:**
- Consumes: Task 5's trust in `x-client-ip`.
- Produces: `x-client-ip` on every request to chat-service; `RESOURCE_EXHAUSTED` without `ErrorInfo` for the rate limit (the frontend in Task 8 relies on this); `grpc-status-details-bin` exposed via CORS.

- [ ] **Step 1: Write the edge check script (the failing test)**

`infra/envoy/check-edge.sh`:

```bash
#!/usr/bin/env bash
# Checks the edge limits in envoy.yaml against a running stack; RESPONDER=echo
# is enough. Run from the repo root: bash infra/envoy/check-edge.sh
set -u
BASE=${BASE:-http://localhost:8080}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
printf '\x00\x00\x00\x00\x00' > "$tmp/empty"         # one empty gRPC-Web frame
head -c 70000 /dev/zero > "$tmp/big"
fail=0

ok()  { echo "ok   $1"; }
bad() { echo "FAIL $1"; fail=1; }

# post PATH FILE [curl args...] prints "<http code> <grpc-status header>".
# A trailers-only response puts grpc-status in the headers; success leaves it
# in the body, so an empty status means the call went through.
post() {
  local path=$1 file=$2; shift 2
  curl -s -o /dev/null -D "$tmp/h" -w '%{http_code}' -X POST "$BASE$path" \
    -H 'content-type: application/grpc-web+proto' -H 'x-grpc-web: 1' "$@" \
    --data-binary @"$file" > "$tmp/code"
  local st
  st=$(tr -d '\r' < "$tmp/h" | awk -F': ' 'tolower($1)=="grpc-status"{print $2}')
  echo "$(cat "$tmp/code") ${st:-none}"
}

# 1. Body limit, before the rate-limit bursts below empty the bucket.
r=$(post /chat.v1.ChatService/Chat "$tmp/big")
case $r in 413*|*\ 8|*\ 13|*\ 14) ok "70 KB body rejected ($r)";; *) bad "70 KB body not rejected ($r)";; esac

# 2. Parked order path is not routed.
r=$(post /orders.OrderService/SendPacket "$tmp/empty")
case $r in 404*|*\ 12) ok "order route removed ($r)";; *) bad "order route still served ($r)";; esac

# 3. CORS: the dev origin is allowed, a foreign one is not.
cors() {
  curl -s -o /dev/null -D - -X OPTIONS "$BASE/chat.v1.ChatService/Chat" \
    -H "Origin: $1" -H 'Access-Control-Request-Method: POST' \
    -H 'Access-Control-Request-Headers: content-type,x-grpc-web' | tr -d '\r' |
    grep -i '^access-control-allow-origin' || true
}
[ -n "$(cors http://localhost:5173)" ] && ok "dev origin allowed" || bad "dev origin not allowed"
[ -z "$(cors https://evil.example)" ] && ok "foreign origin refused" || bad "foreign origin allowed"

# 4. Burst past the per-IP bucket (10, refilling 1 per 2s).
limited=0
for _ in $(seq 1 15); do
  case $(post /chat.v1.ChatService/Warmup "$tmp/empty") in *\ 8) limited=$((limited + 1));; esac
done
[ "$limited" -gt 0 ] && ok "burst rate-limited ($limited of 15)" || bad "burst never rate-limited"

# 5. A forged X-Forwarded-For must not buy a fresh bucket.
spoofed=0
for i in $(seq 1 5); do
  case $(post /chat.v1.ChatService/Warmup "$tmp/empty" -H "X-Forwarded-For: 198.51.100.$i") in *\ 8) spoofed=$((spoofed + 1));; esac
done
[ "$spoofed" -eq 5 ] && ok "forged X-Forwarded-For ignored" || bad "forged X-Forwarded-For got through ($((5 - spoofed)) of 5)"

exit $fail
```

- [ ] **Step 2: Run it against the current config to verify it fails**

Run: `docker compose up --build -d && bash infra/envoy/check-edge.sh`
Expected: FAIL lines for the order route, foreign origin, burst and XFF checks (the body check may already pass on gRPC's own limit; that is fine).

- [ ] **Step 3: Replace the Envoy config**

`infra/envoy/envoy.yaml`:

```yaml
static_resources:
  listeners:
    - name: listener_0
      address:
        socket_address: { address: 0.0.0.0, port_value: 8080 }
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                codec_type: auto
                stat_prefix: ingress_http
                # Every per-visitor limit keys on the address resolved here.
                # Step 7 raises the hop count to the proxies put in front.
                use_remote_address: true
                xff_num_trusted_hops: 0
                route_config:
                  name: local_route
                  # Overwrites any client-sent value; chat-service trusts only this.
                  request_headers_to_add:
                    - header: { key: x-client-ip, value: "%DOWNSTREAM_REMOTE_ADDRESS_WITHOUT_PORT%" }
                      append_action: OVERWRITE_IF_EXISTS_OR_ADD
                  virtual_hosts:
                    - name: local_service
                      domains: ["*"]
                      typed_per_filter_config:
                        envoy.filters.http.cors:
                          "@type": type.googleapis.com/envoy.extensions.filters.http.cors.v3.CorsPolicy
                          allow_origin_string_match:
                            # NOTE: step 7 replaces this with the public origin.
                            - exact: http://localhost:5173
                          allow_methods: POST, OPTIONS
                          allow_headers: keep-alive,user-agent,cache-control,content-type,content-transfer-encoding,x-accept-content-transfer-encoding,x-accept-response-streaming,x-user-agent,x-grpc-web,grpc-timeout,connect-protocol-version,connect-timeout-ms
                          max_age: "1728000"
                          # details-bin carries the refusal reason on a trailers-only error.
                          expose_headers: grpc-status,grpc-message,grpc-status-details-bin
                      routes:
                        - match: { prefix: "/chat.v1.ChatService/" }
                          route:
                            cluster: chat_service_cluster
                            # Defaults to 15s, which truncates streams.
                            timeout: 0s
                            max_stream_duration:
                              grpc_timeout_header_max: 0s
                            rate_limits:
                              - actions:
                                  - remote_address: {}
                          typed_per_filter_config:
                            envoy.filters.http.local_ratelimit:
                              "@type": type.googleapis.com/envoy.extensions.filters.http.local_ratelimit.v3.LocalRateLimit
                              stat_prefix: chat_per_ip
                              # Route-wide bucket, deliberately loose; the per-address
                              # descriptor below is the real limit.
                              token_bucket: { max_tokens: 1000, tokens_per_fill: 1000, fill_interval: 1s }
                              filter_enabled:
                                runtime_key: chat_ratelimit_enabled
                                default_value: { numerator: 100, denominator: HUNDRED }
                              filter_enforced:
                                runtime_key: chat_ratelimit_enforced
                                default_value: { numerator: 100, denominator: HUNDRED }
                              # The frontend reads RESOURCE_EXHAUSTED without ErrorInfo as this limit.
                              rate_limited_as_resource_exhausted: true
                              max_dynamic_descriptors: 10000
                              descriptors:
                                # No value: one bucket per address. 30/min, burst 10.
                                - entries: [{ key: remote_address }]
                                  token_bucket: { max_tokens: 10, tokens_per_fill: 1, fill_interval: 2s }
                        # The parked order path is not routed. Restoring it adds back:
                        #   - match: { prefix: "/" }
                        #     route: { cluster: order_service_cluster, timeout: 0s,
                        #              max_stream_duration: { grpc_timeout_header_max: 0s } }
                http_filters:
                  - name: envoy.filters.http.grpc_web
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.grpc_web.v3.GrpcWeb
                  - name: envoy.filters.http.cors
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.cors.v3.Cors
                  - name: envoy.filters.http.local_ratelimit
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.local_ratelimit.v3.LocalRateLimit
                      stat_prefix: chat_per_ip
                  # Safe to buffer: every request is unary, only Chat's response
                  # streams. Above chat.go's 32 KB history limit, whose error is clearer.
                  - name: envoy.filters.http.buffer
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.buffer.v3.Buffer
                      max_request_bytes: 65536
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
  clusters:
    - name: order_service_cluster
      connect_timeout: 0.25s
      type: STRICT_DNS
      http2_protocol_options: {}
      lb_policy: round_robin
      load_assignment:
        cluster_name: order_service_cluster
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address:
                      address: order-service
                      port_value: 50051
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

`docker-compose.yml`: `image: envoyproxy/envoy:v1.25-latest` → `image: envoyproxy/envoy:v1.39.3`.

- [ ] **Step 4: Run the check to verify it passes**

Make sure `services/chat-service/.env` has the budget variables from Task 6 (the human adds them; agents must not open `.env`). If chat-service is restart-looping with `must be a positive integer`, stop and ask the human to copy the new block from `.env.example`.

Run: `docker compose up --build -d && docker compose logs envoy | tail -20 && bash infra/envoy/check-edge.sh`
Expected: Envoy logs show no config rejection; every line `ok`; exit 0.

If Envoy rejects the config, the error names the field. Check it against the v1.39 API reference for that filter and fix only that field. If the burst check fails because wildcard descriptors are not applied, confirm with `curl -s localhost:8080` is not enough; inspect `docker compose exec envoy wget -qO- localhost:9901/stats | grep chat_per_ip` only if an admin listener is added temporarily, and remove it afterwards. Report what was changed and why.

Also confirm the real browser path: `cd frontend && npm run dev`, send a message, and check that the chat still streams (CORS and `x-client-ip` working end to end).

- [ ] **Step 5: Commit**

```bash
git add infra/envoy docker-compose.yml
git commit -m "feat(envoy): per-IP rate limit, body limit, locked CORS, client address header"
```

---

### Task 8: Frontend refusal copy and balance state

**Files:**
- Create: `frontend/src/components/chat/refusal.ts`
- Test: `frontend/src/components/chat/refusal.test.ts`
- Modify: `frontend/src/components/chat/useChatStream.ts`, `frontend/src/components/chat/useWarmup.ts`
- Test: `frontend/src/components/chat/useChatStream.test.ts`, `frontend/src/components/chat/useWarmup.test.ts`

**Interfaces:**
- Consumes: Task 4 TS `Balance`, `Done.balance`, `WarmupResponse.balance`; Task 5 ErrorInfo contract; Task 7 rate limit as `ResourceExhausted` without details.
- Produces: `describeError(err: unknown): string`, `ErrorInfo` message type; `useChatStream()` and `useWarmup()` both return `balance: Balance | undefined`.

- [ ] **Step 1: Write the failing tests**

`frontend/src/components/chat/refusal.test.ts`:

```ts
import { describe, expect, it } from 'vitest';
import { Code, ConnectError } from '@connectrpc/connect';
import { describeError, ErrorInfo } from './refusal';

// As the transport decodes it: an incoming detail, type name plus bytes.
function refused(reason: string, metadata: Record<string, string> = {}) {
  const info = new ErrorInfo({ reason, domain: 'chat.v1', metadata });
  return new ConnectError('turn refused', Code.ResourceExhausted, undefined, [
    { type: 'google.rpc.ErrorInfo', value: info.toBinary() },
  ]);
}

describe('describeError', () => {
  it('tells a spent visitor when the budget resets', () => {
    const msg = describeError(
      refused('BUDGET_EXHAUSTED', { resets_at: '2026-10-12T00:00:00Z' })
    );
    expect(msg).toMatch(/^You've reached today's limit\. It resets at .+\.$/);
  });

  it('words a spent site differently from a spent visitor', () => {
    const msg = describeError(
      refused('GLOBAL_BUDGET_EXHAUSTED', { resets_at: '2026-10-12T00:00:00Z' })
    );
    expect(msg).toMatch(/^The assistant is resting for today\. Back at .+\.$/);
  });

  it('asks a visitor with too many open streams to finish one', () => {
    expect(describeError(refused('TOO_MANY_STREAMS'))).toBe(
      'Finish your other conversation first.'
    );
  });

  // Envoy's rate limit answers before chat-service and carries no ErrorInfo.
  it('reads ResourceExhausted without a reason as the rate limit', () => {
    const err = new ConnectError('rate limited', Code.ResourceExhausted);
    expect(describeError(err)).toBe('Slow down a moment and try again.');
  });

  it('passes other errors through unchanged', () => {
    expect(describeError(new Error('boom'))).toBe('boom');
    expect(describeError('nope')).toBe('Unknown error');
  });
});
```

In `useChatStream.test.ts`, add (reusing the file's existing `mockChat` and `deltaChunk` helpers):

```ts
import { Balance } from '../../gen/chat_pb';

describe('useChatStream balance', () => {
  beforeEach(() => mockChat.mockReset());

  it('keeps the balance from the done frame', async () => {
    const balance = new Balance({ tokensUsed: 400, tokensRemaining: 19600, resetsAt: '2026-10-12T00:00:00Z' });
    mockChat.mockReturnValue(
      (async function* () {
        yield deltaChunk('hi');
        yield new ChatChunk({ event: { case: 'done', value: new Done({ balance }) } });
      })() as never
    );

    const { result } = renderHook(() => useChatStream());
    await act(async () => {
      await result.current.send('hello');
    });
    expect(result.current.balance?.tokensRemaining).toBe(19600);
  });
});
```

Match the cast used by existing tests in that file when they stub `mockChat.mockReturnValue` with an async generator; if they use a helper, use it instead of `as never`.

In `useWarmup.test.ts`, add:

```ts
import { Balance } from '../../gen/chat_pb';

it('exposes the balance the warmup reported', async () => {
  mockWarmup.mockResolvedValue(
    new WarmupResponse({ balance: new Balance({ tokensRemaining: 1234 }) })
  );
  const { result } = renderHook(() => useWarmup(0));
  await waitFor(() => expect(result.current.balance?.tokensRemaining).toBe(1234));
});
```

(inside the existing `describe('useWarmup', ...)` block).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd frontend && npm test`
Expected: FAIL: `./refusal` not found; `balance` undefined.

- [ ] **Step 3: Implement**

`frontend/src/components/chat/refusal.ts`:

```ts
import { Code, ConnectError } from '@connectrpc/connect';
import { proto3, ScalarType } from '@bufbuild/protobuf';

// google.rpc.ErrorInfo, declared by hand: one message is not worth a
// googleapis dependency in buf.gen.yaml.
export const ErrorInfo = proto3.makeMessageType('google.rpc.ErrorInfo', () => [
  { no: 1, name: 'reason', kind: 'scalar', T: ScalarType.STRING },
  { no: 2, name: 'domain', kind: 'scalar', T: ScalarType.STRING },
  {
    no: 3,
    name: 'metadata',
    kind: 'map',
    K: ScalarType.STRING,
    V: { kind: 'scalar', T: ScalarType.STRING },
  },
]);

const clock = new Intl.DateTimeFormat(undefined, {
  hour: '2-digit',
  minute: '2-digit',
});

// Copy keyed on chat-service's refusal reasons; read by people evaluating the
// work, so it is reviewed like the guard's refusal.
export function describeError(err: unknown): string {
  if (!(err instanceof ConnectError) || err.code !== Code.ResourceExhausted) {
    return err instanceof Error ? err.message : 'Unknown error';
  }
  const info = err.findDetails(ErrorInfo)[0];
  const resetsAt = (info?.metadata as Record<string, string> | undefined)?.resets_at;
  const at = resetsAt ? clock.format(new Date(resetsAt)) : 'midnight UTC';
  switch (info?.reason) {
    case 'TOO_MANY_STREAMS':
      return 'Finish your other conversation first.';
    case 'BUDGET_EXHAUSTED':
      return `You've reached today's limit. It resets at ${at}.`;
    case 'GLOBAL_BUDGET_EXHAUSTED':
      return `The assistant is resting for today. Back at ${at}.`;
    default:
      // Envoy's per-IP limit, which answers before chat-service.
      return 'Slow down a moment and try again.';
  }
}
```

`useChatStream.ts`:
- `import { Balance, Role } from '../../gen/chat_pb';` and `import { describeError } from './refusal';`
- `const [balance, setBalance] = useState<Balance | undefined>();`
- In the `done` branch: `const { signature, balance: next } = chunk.event.value;` then `if (next) setBalance(next);` before the existing `setMessages`.
- In `catch`: `setError(describeError(err));`
- Return `{ messages, streaming, error, send, stop, balance }`.

`useWarmup.ts`:
- `import type { Balance } from '../../gen/chat_pb';`
- `const [balance, setBalance] = useState<Balance | undefined>();`
- `.then((res) => { setBalance(res.balance); setStatus('ready'); })`
- Return `{ status, welcome, retry, balance }`.

- [ ] **Step 4: Run tests, lint, build**

Run: `cd frontend && npm test && npm run lint && npm run build`
Expected: all pass. If `npm run lint` flags `refusal.ts` for the `metadata` cast, keep the cast and narrow it the way the lint rule asks; do not disable the rule.

- [ ] **Step 5: Commit**

```bash
git add frontend/src/components/chat
git commit -m "feat(frontend): refusal copy per reason and budget balance in state"
```

---

### Task 9: Alert rules and Alertmanager

**Files:**
- Create: `infra/prometheus/rules/chat.yml`
- Create: `infra/prometheus/rules-test/chat.test.yml`
- Create: `infra/alertmanager/alertmanager.yml`
- Modify: `infra/prometheus/prometheus.yml`
- Modify: `docker-compose.yml` (prometheus volumes/depends_on, new `alertmanager` service)
- Modify: `infra/grafana/provisioning/datasources/datasources.yaml`

**Interfaces:**
- Consumes: metrics `chat_tokens_total` (existing), `chat_budget_refusals_total{reason}` (Task 1), `chat_guard_checks_total{verdict}` (existing).
- Produces: alerts `TokenSpendPace`, `GlobalBudgetExhausted`, `BudgetRefusalSpike`, `GuardDegraded`; Alertmanager on `:9093`.

- [ ] **Step 1: Write the failing rule tests**

`infra/prometheus/rules-test/chat.test.yml` (outside `rules/` so Prometheus never loads it as rules):

```yaml
rule_files:
  - ../rules/chat.yml

evaluation_interval: 1m

tests:
  - interval: 1h
    input_series:
      - series: 'chat_tokens_total{direction="input"}'
        values: '0+20000x24'
      - series: 'chat_tokens_total{direction="output"}'
        values: '0+1000x24'
    alert_rule_test:
      - eval_time: 24h
        alertname: TokenSpendPace
        exp_alerts:
          - exp_labels: { severity: warning }
            exp_annotations:
              summary: Token spend over the last 24h passed 70% of the global daily budget.

  - interval: 1h
    input_series:
      - series: 'chat_tokens_total{direction="input"}'
        values: '0+1000x24'
    alert_rule_test:
      - eval_time: 24h
        alertname: TokenSpendPace
        exp_alerts: []

  - interval: 1m
    input_series:
      - series: 'chat_budget_refusals_total{reason="GLOBAL_BUDGET_EXHAUSTED"}'
        values: '0 0 1'
    alert_rule_test:
      - eval_time: 2m
        alertname: GlobalBudgetExhausted
        exp_alerts:
          - exp_labels: { severity: critical }
            exp_annotations:
              summary: The global token budget or visitor cap is spent; new turns are refused until midnight UTC.

  - interval: 1m
    input_series:
      - series: 'chat_budget_refusals_total{reason="BUDGET_EXHAUSTED"}'
        values: '0+10x40'
    alert_rule_test:
      - eval_time: 25m
        alertname: BudgetRefusalSpike
        exp_alerts:
          - exp_labels: { severity: warning }
            exp_annotations:
              summary: More than 50 turns refused in 10 minutes; check for abuse or a budget set too low.

  - interval: 1m
    input_series:
      - series: 'chat_guard_checks_total{verdict="error"}'
        values: '0+2x40'
      - series: 'chat_guard_checks_total{verdict="pass"}'
        values: '0+8x40'
    alert_rule_test:
      - eval_time: 25m
        alertname: GuardDegraded
        exp_alerts:
          - exp_labels: { severity: warning }
            exp_annotations:
              summary: Over 10% of guard checks are failing; those turns get the fixed unavailable reply.
```

- [ ] **Step 2: Run them to verify they fail**

Run from the repo root (Git Bash needs `MSYS_NO_PATHCONV` and a Windows path for the mount):

```bash
MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd -W)/infra/prometheus:/p" --entrypoint promtool prom/prometheus:latest test rules /p/rules-test/chat.test.yml
```

Expected: FAIL, rule file `../rules/chat.yml` not found.

- [ ] **Step 3: Write the rules**

`infra/prometheus/rules/chat.yml`:

```yaml
groups:
  - name: chat-cost
    rules:
      # NOTE: 350000 is 70% of BUDGET_TOKENS_GLOBAL in services/chat-service/.env.example.
      # Change both together. Rolling 24h, not the ledger's UTC day: it can
      # only fire earlier, and it survives a chat-service restart.
      - alert: TokenSpendPace
        expr: sum(increase(chat_tokens_total[24h])) > 350000
        for: 5m
        labels: { severity: warning }
        annotations:
          summary: Token spend over the last 24h passed 70% of the global daily budget.

      - alert: GlobalBudgetExhausted
        expr: sum(increase(chat_budget_refusals_total{reason="GLOBAL_BUDGET_EXHAUSTED"}[10m])) > 0
        labels: { severity: critical }
        annotations:
          summary: The global token budget or visitor cap is spent; new turns are refused until midnight UTC.

      - alert: BudgetRefusalSpike
        expr: sum(increase(chat_budget_refusals_total[10m])) > 50
        for: 10m
        labels: { severity: warning }
        annotations:
          summary: More than 50 turns refused in 10 minutes; check for abuse or a budget set too low.

      - alert: GuardDegraded
        expr: |
          sum(rate(chat_guard_checks_total{verdict="error"}[10m]))
            / sum(rate(chat_guard_checks_total[10m])) > 0.1
        for: 10m
        labels: { severity: warning }
        annotations:
          summary: Over 10% of guard checks are failing; those turns get the fixed unavailable reply.
```

- [ ] **Step 4: Run the rule tests to verify they pass**

Run the Step 2 command again.
Expected: `SUCCESS`. If `TokenSpendPace`'s positive case fails on extrapolation, raise the input series slope rather than lowering the threshold.

- [ ] **Step 5: Wire Prometheus, Alertmanager and Grafana**

`infra/alertmanager/alertmanager.yml`:

```yaml
# Step 7 picks a real receiver. Until then alerts show at :9093 and in Grafana,
# and are delivered nowhere.
route:
  receiver: none
  group_by: [alertname]
  group_wait: 30s
  group_interval: 5m
  repeat_interval: 4h
receivers:
  - name: none
```

`infra/prometheus/prometheus.yml`, after the `global:` block:

```yaml
rule_files:
  - /etc/prometheus/rules/*.yml

alerting:
  alertmanagers:
    - static_configs:
        - targets: ['alertmanager:9093']
```

`docker-compose.yml`, prometheus service: add volume `- ./infra/prometheus/rules:/etc/prometheus/rules` and

```yaml
    depends_on:
      - alertmanager
```

New service, next to prometheus:

```yaml
  alertmanager:
    image: prom/alertmanager:v0.28.1
    container_name: alertmanager
    volumes:
      - ./infra/alertmanager/alertmanager.yml:/etc/alertmanager/alertmanager.yml
    ports:
      - "9093:9093"
    networks:
      - micro-network
```

`infra/grafana/provisioning/datasources/datasources.yaml`, append:

```yaml
  - name: Alertmanager
    type: alertmanager
    uid: alertmanager
    url: http://alertmanager:9093
    jsonData:
      implementation: prometheus
```

- [ ] **Step 6: Verify against the running stack**

Run: `docker compose up -d && curl -s localhost:9090/api/v1/rules | grep -o '"name":"[A-Za-z]*"' && curl -s localhost:9090/api/v1/alertmanagers && curl -s -o /dev/null -w '%{http_code}\n' localhost:9093/-/ready`
Expected: the four alert names listed; one active Alertmanager `http://alertmanager:9093/api/v2/alerts`; `200`.

- [ ] **Step 7: Commit**

```bash
git add infra/prometheus infra/alertmanager infra/grafana docker-compose.yml
git commit -m "feat(alerting): spend, refusal and guard alerts through Alertmanager"
```

---

### Task 10: Documentation

**Files:**
- Modify: `docs/roadmap.md`
- Modify: `CLAUDE.md`

- [ ] **Step 1: Roadmap**

In `docs/roadmap.md`: set row 6 to `Done`; move section "6. Web security" from **Next** to **Done** (after 5), rewritten as shipped:

```markdown
### 6. Web security
A per-token API's spend cap is global: one visitor who reaches it takes the
assistant down for everyone. The budget is per visitor first.
- Per-visitor and site-wide daily token budgets in chat-service, keyed on the
  client address Envoy resolves; charged after each turn with the provider's
  reported usage, condense included; balance returned on `Done` and `Warmup`
- Embedding tokens counted in `chat_embed_tokens_total`, not charged
- Concurrent stream caps, refused at once rather than queued
- `max_tokens` per reply, bounding how far one turn overshoots a budget
- Envoy v1.39: per-IP rate limit, 64 KB body limit, CORS locked to one origin,
  parked order route removed
- Prometheus alert rules through Alertmanager: spend pace, cap hits, refusal
  spikes, a degraded guard
- In memory by choice: a restart resets budgets; the provider cap backs it
```

Remove "Keep crawlers from triggering `Warmup`" (covered by `warmCache` and the rate limit). In section 7 add bullets: `xff_num_trusted_hops` and the CORS origin set for the chosen front proxy and domain; Alertmanager receiver; firewall closes `:9093` with the other observability ports. In section 11 add: "A shared budget ledger before chat-service gets a second replica." Update *Last updated*.

- [ ] **Step 2: CLAUDE.md**

- **Parked** paragraph: "Their code, Envoy route and scrape jobs stay" → "Their code, Envoy cluster and scrape jobs stay; the route is commented out in `envoy.yaml`".
- Endpoints line: add "Alertmanager `:9093`".
- Under **Architecture**, after **LLM security**, add:

```markdown
**Budgets.** Every per-visitor limit keys on `x-client-ip`, which Envoy overwrites with the address it resolved (`use_remote_address` + `xff_num_trusted_hops`); chat-service refuses a request without it rather than pooling it. `internal/budget` checks stream caps and today's tokens before the turn and debits the provider-reported usage after, on every exit path. Refusals are `ResourceExhausted` with an `ErrorInfo` reason the frontend maps to copy; Envoy's per-IP limit is the `ResourceExhausted` without one. Limits are required env vars with no unlimited value; the alert threshold in `infra/prometheus/rules/chat.yml` tracks `BUDGET_TOKENS_GLOBAL`.
```

- Commands section, after the promptfoo line:

```bash
bash infra/envoy/check-edge.sh                         # edge limits, stack up
MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd -W)/infra/prometheus:/p" --entrypoint promtool prom/prometheus:latest test rules /p/rules-test/chat.test.yml
```

- [ ] **Step 3: Commit**

```bash
git add docs/roadmap.md CLAUDE.md
git commit -m "docs: record web security in the roadmap and CLAUDE.md"
```
