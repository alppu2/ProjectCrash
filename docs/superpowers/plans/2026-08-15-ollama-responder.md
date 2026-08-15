# OpenAI-Compatible Responder Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace `chat-service`'s hardcoded `EchoResponder` with an env-selected `OpenAIResponder` that streams real model replies, backed locally by an Ollama container and later by a hosted open-model API with no code change.

**Architecture:** A new `OpenAIResponder` fills the existing `Responder` seam by POSTing to `{LLM_BASE_URL}/chat/completions` and turning the server-sent-event stream into `emit` calls plus a `Usage`. Ollama joins `docker-compose.yml` as a service on `micro-network`, so `chat-service` reaches it by DNS name exactly as it will reach Groq or vLLM later. Nothing above the seam changes: `proto/chat.proto`, the `Chat` RPC handler's control flow, `envoy.yaml`, and every frontend file stay as they are. `main.go` gains a `newResponder()` factory so `RESPONDER=echo` (the default) keeps CI and load tests dependency-free.

**Tech Stack:** Go 1.26.3, stdlib `net/http` + `encoding/json` + `bufio` (no new dependencies), `promauto` for metrics, `httptest` for tests, Docker Compose with `gpus: all`.

**Spec:** `docs/superpowers/specs/2026-08-09-ollama-responder-design.md` — **read the `## Amendment 2026-08-15` section at the bottom first.** It reverses two decisions in the table at the top: Ollama now runs in compose, and the endpoint is OpenAI-compatible `/v1` rather than Ollama's native `/api/chat`. Where the amendment and the original body conflict, the amendment wins.

## Global Constraints

- Go module is `chat-service`, Go `1.26.3`. Each service directory is its own module — `cd chat-service` before any `go` command; there is no workspace.
- **No new Go dependencies.** `go.mod` must not change. Only `bufio`, `bytes`, `context`, `encoding/json`, `errors`, `fmt`, `io`, `net/http`, `net/url`, `strings`, `time`, plus already-present `google.golang.org/grpc` and `prometheus/client_golang`.
- **Tests must be hermetic.** No test may contact Ollama or any hosted API. `cd chat-service && go test ./...` must pass with the whole compose stack down.
- **`chat.go`'s single-metrics-site invariant holds:** exactly one `chatStreamsTotal.Inc()` and one `chatStreamDuration.Observe()` per `Chat` call, both inside the existing single deferred func. Do not add a second Inc/Observe pair for those two metrics on any new exit path. The new metrics (`chat_tokens_total`, `chat_provider_errors_total`, `chat_time_to_first_token_seconds`) are separate collectors, not covered by that invariant.
- **Client disconnects are not provider errors.** `context.Canceled` must be returned unwrapped so the existing `classifyOutcome` maps it to `cancelled`, and must NOT increment `chat_provider_errors_total`.
- **Provider unavailable fails the RPC.** Never fall back to echo at runtime; a degraded reply must not be indistinguishable from a real one.
- **Partial `Usage` survives errors.** `finish_reason` and `usage` arrive in separate SSE frames, so accumulate into one `Usage` var as frames land and return it on every exit path, error included. See the `Usage` doc comment in `responder.go`.
- Every config value has an in-code default, so an empty `.env` still boots as echo.
- Exact default values: `RESPONDER=echo`, `LLM_BASE_URL=http://ollama:11434/v1`, `LLM_MODEL=llama3.2:3b`, `LLM_API_KEY=` (empty), `ResponseHeaderTimeout=120s`.
- Time-to-first-token buckets, verbatim: `.1, .25, .5, 1, 2, 5, 10, 30, 60, 120`.
- `reason` label values, complete set: `unreachable | model_missing | auth_error | rate_limited | http_error | decode_error | config_error`.
- Generated protobuf code (`chat-service/chat/`) is not touched by this plan.
- `order-service` scaling must keep working: do not add a `container_name` or host port mapping to it, and do not touch its compose block.

### Spec addenda decided during planning

1. The amendment's error table lists six reasons; implementation needs a seventh, `config_error`, for two paths Go forces us to handle but which cannot fire at request time in practice: `json.Marshal` of a struct of strings and bools, and `http.NewRequestWithContext` with an already-validated URL. Both map to `codes.Internal`. `newResponder` validates `LLM_BASE_URL` at startup so a malformed URL is a boot failure, not a per-request `config_error`.
2. Non-200 responses surface the provider's own error text, bounded to 2KB, rather than only a status code. Not in the spec, but the deployed target is likely OpenAI's `gpt-4o-mini`, whose `{"error":{"message":...}}` body says things like "This model's maximum context length is..." that a bare `HTTP 400` would throw away. Error strings reach the browser, and per CLAUDE.md they are portfolio surface.
3. The model name appears in two places: the `ollama` service's pull/healthcheck in `docker-compose.yml`, and `LLM_MODEL` in `chat-service/.env`. A single source would mean introducing a root-level `.env` for interpolation, a convention this repo does not otherwise use. The two are left coupled by a comment instead; drift produces the loud, self-explaining `model_missing` error rather than silent breakage.

---

### Task 1: Bring up Ollama in compose and verify the real SSE contract

This task runs first on purpose. Every later test asserts against captured frames, and the amendment flags one assumption — `stream_options.include_usage` on Ollama's compat layer — that has not been confirmed the way the native field names were. Verifying the wire format before writing the decoder is what keeps this plan's test constants real rather than transcribed from documentation.

**Files:**
- Modify: `docker-compose.yml` (new `ollama` service, new named volume)

**Interfaces:**
- Consumes: nothing.
- Produces: an `ollama` service resolvable at `http://ollama:11434` on `micro-network`, healthy only once `llama3.2:3b` is present; the named volume `ollama-models`; and a recorded transcript of real SSE frames that Task 3's test constants must match.

- [ ] **Step 1: Add the Ollama service to `docker-compose.yml`**

Add this service block. Place it near `chat-service` rather than at the end, so the two read together:

```yaml
  ollama:
    image: ollama/ollama:0.32.6
    container_name: ollama
    # No published host port: chat-service reaches this over micro-network, and
    # keeping it off the host avoids exposing an unauthenticated inference API.
    # Use `docker compose exec ollama ...` for debugging.
    volumes:
      - ollama-models:/root/.ollama
    environment:
      # Keep the model resident between turns; a cold VRAM load costs ~33s.
      OLLAMA_KEEP_ALIVE: 30m
    # Pull on every start so a fresh volume self-provisions. The pull is a no-op
    # once the model is in the volume. NOTE: this model name is duplicated in
    # chat-service/.env as LLM_MODEL — change both together, or requests fail
    # with reason="model_missing".
    entrypoint:
      - /bin/sh
      - -c
      - |
        ollama serve &
        until ollama list >/dev/null 2>&1; do sleep 1; done
        ollama pull llama3.2:3b
        wait
    # Healthy only once the model is actually pulled, not merely when the API
    # answers — otherwise chat-service starts and 404s on its first turn.
    healthcheck:
      test: ["CMD-SHELL", "ollama list | grep -q llama3.2:3b"]
      interval: 10s
      timeout: 5s
      retries: 60
      start_period: 600s
    gpus: all
    networks:
      - micro-network
```

Add `ollama-models:` to the top-level `volumes:` block, following whatever style the existing volume entries use.

- [ ] **Step 2: Start it and wait for healthy**

```bash
docker compose up -d ollama
docker compose logs -f ollama          # watch the ~2GB pull; Ctrl-C when done
docker compose ps ollama               # repeat until STATUS shows (healthy)
```

Expected: the pull completes, then STATUS becomes `Up (healthy)`. First run takes several minutes.

If startup fails with a GPU error, confirm the runtime is really there — `docker info --format '{{json .Runtimes}}'` must list `nvidia`. If it does not, drop the `gpus: all` line and continue on CPU; everything else in this plan is unaffected, only slower.

- [ ] **Step 3: Confirm GPU acceleration is actually in use**

```bash
docker compose exec ollama nvidia-smi --query-gpu=name,memory.used --format=csv
docker compose exec ollama ollama run llama3.2:3b "say hi" --verbose
```

Expected: `nvidia-smi` names the GTX 1060, and the `--verbose` output reports an `eval rate` in the tens of tokens per second. Single-digit tokens per second means it fell back to CPU — note that in the report; it does not block the rest of the plan.

- [ ] **Step 4: Capture the real SSE stream — this is the verification gate**

Ollama publishes no host port, so drive it from a throwaway container on the same network. The `ollama/ollama` image ships neither `curl` nor `wget`, and `docker compose run` conflicts with the service's `container_name`, so use a plain `docker run` with an image that has a HTTP client.

First get the network's real name — Compose prefixes it with the project directory:

```bash
docker network ls --filter name=micro-network --format '{{.Name}}'
```

Then, substituting that name (expected `projectcrash_micro-network`):

```bash
docker run --rm --network projectcrash_micro-network curlimages/curl:latest \
  -sN -X POST http://ollama:11434/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"llama3.2:3b","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"count: one two"}]}'
```

`-N` disables curl's output buffering, so frames appear as they arrive rather than in one block at the end.

Read the output against the four things Task 3 depends on, and write down which hold:

1. Lines are `data: `-prefixed JSON, terminated by a literal `data: [DONE]`.
2. Text deltas appear at `choices[0].delta.content`.
3. A frame carries `choices[0].finish_reason` (expected `"stop"`) with an empty `delta`.
4. **The assumption under test:** a trailing frame carries `usage` with `prompt_tokens` and `completion_tokens`, and an empty `choices` array.

If (4) does not appear, Ollama's compat layer ignores `stream_options`. That is a known acceptable outcome: keep the `Usage` mapping exactly as Task 3 writes it, note that `chat_tokens_total` will read zero locally and start reporting against a hosted provider, and adjust only the one Task 5 test that asserts local token counts. Do not redesign.

If (1), (2) or (3) differ from what Task 3's constants and struct tags assume, fix the constants and tags in Task 3 to match what you captured, and say so in the report. Real frames win over this document.

- [ ] **Step 5: Verify a 404 for a missing model**

```bash
docker run --rm --network projectcrash_micro-network curlimages/curl:latest \
  -s -o /dev/null -w '%{http_code}\n' -X POST http://ollama:11434/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"nope","stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

Expected: prints `404`. Task 4 maps that status to `reason="model_missing"`. If the compat layer returns 400 instead, record the real status and adjust Task 4's status check and its test.

- [ ] **Step 6: Commit**

```bash
git add docker-compose.yml
git commit -m "feat(compose): run Ollama as a service with a GPU and a model volume"
```

- [ ] **Step 7: Report the transcript**

Paste the captured frames into the completion report, with a yes/no on each of the four contract points from Step 4 and the observed eval rate. Later tasks read this.

---

### Task 2: `validateHistory` rejects `ROLE_UNSPECIFIED` anywhere in history

Role mapping in Task 3 is total only if `ROLE_UNSPECIFIED` never reaches it. Today `validateHistory` rejects an unset role only on the **last** message (via the `!= Role_ROLE_USER` check at `chat.go:131`); a mid-history message with no role passes and would silently be sent as `"user"`, corrupting the conversation.

**Files:**
- Modify: `chat-service/chat.go:116-138` (`validateHistory`)
- Test: `chat-service/chat_test.go` (new cases in `TestValidateHistoryBounds`, which already lives at line 174)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: the guarantee that every `*chatpb.Message` reaching a `Responder` has `Role` equal to `Role_ROLE_USER` or `Role_ROLE_ASSISTANT`. Task 3's `toOpenAIMessages` relies on it.

- [ ] **Step 1: Write the failing test**

Add these two cases to the `tests` slice in `TestValidateHistoryBounds` in `chat-service/chat_test.go`, after the existing `"content bytes just under limit is accepted"` case:

```go
		{
			name: "mid-history message with unset role is rejected",
			msgs: []*chatpb.Message{
				{Role: chatpb.Role_ROLE_UNSPECIFIED, Content: "who am I"},
				{Role: chatpb.Role_ROLE_USER, Content: "hi"},
			},
			wantErr: true,
		},
		{
			name: "user and assistant roles are accepted",
			msgs: []*chatpb.Message{
				{Role: chatpb.Role_ROLE_USER, Content: "hi"},
				{Role: chatpb.Role_ROLE_ASSISTANT, Content: "hello"},
				{Role: chatpb.Role_ROLE_USER, Content: "again"},
			},
			wantErr: false,
		},
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd chat-service && go test -run TestValidateHistoryBounds ./...`

Expected: FAIL on `mid-history message with unset role is rejected` with `validateHistory() code = OK, want InvalidArgument (err = <nil>)`. The second case should already pass.

- [ ] **Step 3: Write minimal implementation**

In `chat-service/chat.go`, inside `validateHistory`, insert this loop immediately after the `maxHistoryBytes` check and before `last := msgs[len(msgs)-1]`:

```go
	// Every message needs a role a provider can map. The OpenAI-compatible
	// wire format takes "user"/"assistant" strings and has no equivalent of an
	// unset role, so a ROLE_UNSPECIFIED message anywhere in the history would
	// have to be guessed at by a Responder. Reject it here instead.
	for i, m := range msgs {
		if m.GetRole() == chatpb.Role_ROLE_UNSPECIFIED {
			return status.Errorf(codes.InvalidArgument, "message %d has no role; every message must be ROLE_USER or ROLE_ASSISTANT", i)
		}
	}
```

Keep it as a separate pass after the byte accounting, so an oversized history still reports the size problem first and the existing bounds cases are unaffected.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd chat-service && go test ./... && go vet ./...`

Expected: PASS, no vet output. `TestChatRejectsInvalidHistory`'s `"last message role unset"` case still passes — it now trips this loop instead of the last-message role check, which returns the same status code.

- [ ] **Step 5: Commit**

```bash
git add chat-service/chat.go chat-service/chat_test.go
git commit -m "fix(chat): reject messages with an unset role anywhere in history"
```

---

### Task 3: `OpenAIResponder` streaming happy path

**Files:**
- Create: `chat-service/openai.go`
- Create: `chat-service/openai_test.go`

**Interfaces:**
- Consumes: `Responder` and `Usage` from `chat-service/responder.go`; the role guarantee from Task 2; the verified frame shapes from Task 1 Step 4.
- Produces:
  - `type OpenAIResponder struct { BaseURL string; Model string; APIKey string; Client *http.Client }` implementing `Stream(ctx context.Context, req *chatpb.ChatRequest, emit func(delta string) error) (Usage, error)`.
  - `const defaultLLMBaseURL = "http://ollama:11434/v1"`, `const defaultLLMModel = "llama3.2:3b"`.
  - `func newLLMClient() *http.Client`.
  - Test helpers in `openai_test.go`: `sseServer(t *testing.T, status int, frames ...string) *httptest.Server`, `newTestLLM(baseURL string) *OpenAIResponder`, and the frame constants `frameHel`, `frameLo`, `frameFinish`, `frameUsage`, `frameDone`.
  - Task 4 adds error paths to the same file and calls `providerError`, which Task 4 defines.

- [ ] **Step 1: Write the failing tests**

Create `chat-service/openai_test.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	chatpb "chat-service/chat"
)

// Frames as captured from a real provider in Task 1 Step 4. Text deltas arrive
// at choices[0].delta.content; finish_reason arrives on a frame whose delta is
// empty; token counts arrive in a trailing usage-only frame with no choices.
const (
	frameHel    = `data: {"choices":[{"index":0,"delta":{"content":"Hel"}}]}`
	frameLo     = `data: {"choices":[{"index":0,"delta":{"content":"lo"}}]}`
	frameFinish = `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	frameUsage  = `data: {"choices":[],"usage":{"prompt_tokens":26,"completion_tokens":298}}`
	frameDone   = `data: [DONE]`
)

// sseServer serves the given frames as a server-sent-event stream, flushing
// each so a reading client sees them arrive separately rather than in one
// buffer. Frames are written verbatim, so a test can pass malformed input.
func sseServer(t *testing.T, status int, frames ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		for _, f := range frames {
			io.WriteString(w, f+"\n\n")
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestLLM(baseURL string) *OpenAIResponder {
	return &OpenAIResponder{BaseURL: baseURL, Model: "llama3.2:3b", Client: &http.Client{}}
}

func TestOpenAIResponderStreamsDeltasAndUsage(t *testing.T) {
	srv := sseServer(t, http.StatusOK, frameHel, frameLo, frameFinish, frameUsage, frameDone)
	r := newTestLLM(srv.URL)

	var got []string
	req := &chatpb.ChatRequest{Messages: userHistory("hi")}
	usage, err := r.Stream(context.Background(), req, func(d string) error {
		got = append(got, d)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream() error = %v, want nil", err)
	}

	// Deltas travel verbatim: the provider already includes leading spaces, so
	// unlike EchoResponder there is no separator to reconstruct.
	if want := []string{"Hel", "lo"}; !slices.Equal(got, want) {
		t.Errorf("deltas = %q, want %q (empty-delta frames must be skipped)", got, want)
	}
	if usage.StopReason != "stop" {
		t.Errorf("StopReason = %q, want %q", usage.StopReason, "stop")
	}
	if usage.InputTokens != 26 {
		t.Errorf("InputTokens = %d, want 26", usage.InputTokens)
	}
	if usage.OutputTokens != 298 {
		t.Errorf("OutputTokens = %d, want 298", usage.OutputTokens)
	}
}

// A provider that ignores stream_options sends no usage frame. The stream must
// still succeed with whatever it did report — Ollama's compat layer may behave
// this way, and a missing token count is not a failed turn.
func TestOpenAIResponderToleratesMissingUsageFrame(t *testing.T) {
	srv := sseServer(t, http.StatusOK, frameHel, frameFinish, frameDone)
	r := newTestLLM(srv.URL)

	req := &chatpb.ChatRequest{Messages: userHistory("hi")}
	usage, err := r.Stream(context.Background(), req, func(string) error { return nil })
	if err != nil {
		t.Fatalf("Stream() error = %v, want nil", err)
	}
	if usage.StopReason != "stop" {
		t.Errorf("StopReason = %q, want %q", usage.StopReason, "stop")
	}
	if usage.InputTokens != 0 || usage.OutputTokens != 0 {
		t.Errorf("tokens = (%d, %d), want (0, 0)", usage.InputTokens, usage.OutputTokens)
	}
}

// Partial usage must survive an error: finish_reason and usage arrive in
// separate frames, so a stream that dies after them has still reported them.
// This is what the Usage doc comment in responder.go requires.
func TestOpenAIResponderKeepsUsageOnMidStreamFailure(t *testing.T) {
	srv := sseServer(t, http.StatusOK, frameHel, frameFinish, frameUsage, `data: {"choices":[`)
	r := newTestLLM(srv.URL)

	req := &chatpb.ChatRequest{Messages: userHistory("hi")}
	usage, err := r.Stream(context.Background(), req, func(string) error { return nil })
	if err == nil {
		t.Fatal("Stream() error = nil, want a decode error")
	}
	if usage.OutputTokens != 298 {
		t.Errorf("OutputTokens = %d, want 298 — usage already reported must not be discarded", usage.OutputTokens)
	}
	if usage.StopReason != "stop" {
		t.Errorf("StopReason = %q, want %q", usage.StopReason, "stop")
	}
}

func TestOpenAIResponderRequestShape(t *testing.T) {
	type capturedMessage struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type captured struct {
		Model         string            `json:"model"`
		Stream        bool              `json:"stream"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		Messages []capturedMessage `json:"messages"`
	}

	var body captured
	var method, path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, auth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, frameDone+"\n\n")
	}))
	t.Cleanup(srv.Close)

	r := newTestLLM(srv.URL)
	req := &chatpb.ChatRequest{Messages: []*chatpb.Message{
		{Role: chatpb.Role_ROLE_USER, Content: "first"},
		{Role: chatpb.Role_ROLE_ASSISTANT, Content: "reply"},
		{Role: chatpb.Role_ROLE_USER, Content: "second"},
	}}
	if _, err := r.Stream(context.Background(), req, func(string) error { return nil }); err != nil {
		t.Fatalf("Stream() error = %v, want nil", err)
	}

	if method != http.MethodPost || path != "/chat/completions" {
		t.Errorf("request = %s %s, want POST /chat/completions", method, path)
	}
	if body.Model != "llama3.2:3b" {
		t.Errorf("model = %q, want %q", body.Model, "llama3.2:3b")
	}
	if !body.Stream {
		t.Error("stream = false, want true — a non-streaming request blocks until the whole reply is generated")
	}
	if !body.StreamOptions.IncludeUsage {
		t.Error("stream_options.include_usage = false, want true — without it there are no token counts")
	}
	// An empty APIKey must send no header at all: a local Ollama needs none,
	// and "Bearer " with nothing after it is a malformed credential.
	if auth != "" {
		t.Errorf("Authorization = %q, want it absent when APIKey is empty", auth)
	}
	want := []capturedMessage{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "reply"},
		{Role: "user", Content: "second"},
	}
	if !slices.Equal(body.Messages, want) {
		t.Errorf("messages = %+v, want %+v", body.Messages, want)
	}
}

func TestOpenAIResponderSendsBearerTokenWhenSet(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, frameDone+"\n\n")
	}))
	t.Cleanup(srv.Close)

	r := newTestLLM(srv.URL)
	r.APIKey = "gsk_secret"

	req := &chatpb.ChatRequest{Messages: userHistory("hi")}
	if _, err := r.Stream(context.Background(), req, func(string) error { return nil }); err != nil {
		t.Fatalf("Stream() error = %v, want nil", err)
	}
	if auth != "Bearer gsk_secret" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer gsk_secret")
	}
}

func TestOpenAIResponderPropagatesEmitError(t *testing.T) {
	srv := sseServer(t, http.StatusOK, frameHel, frameLo, frameFinish, frameDone)
	r := newTestLLM(srv.URL)
	sentinel := errors.New("send failed")

	req := &chatpb.ChatRequest{Messages: userHistory("hi")}
	_, err := r.Stream(context.Background(), req, func(string) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("Stream() error = %v, want %v unwrapped", err, sentinel)
	}
}
```

`userHistory` already exists in `responder_test.go`, same package — do not redefine it.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd chat-service && go test -run TestOpenAIResponder ./...`

Expected: FAIL to compile with `undefined: OpenAIResponder`.

- [ ] **Step 3: Write minimal implementation**

Create `chat-service/openai.go`:

```go
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	chatpb "chat-service/chat"
)

const (
	// Ollama runs as a compose service, so this is a DNS name on
	// micro-network — the same shape as a hosted provider's URL. Swapping to
	// Groq, OpenRouter or a self-hosted vLLM is LLM_BASE_URL plus LLM_API_KEY,
	// with no code change.
	defaultLLMBaseURL = "http://ollama:11434/v1"
	defaultLLMModel   = "llama3.2:3b"
)

// OpenAIResponder streams a reply from any provider speaking the
// OpenAI-compatible /chat/completions API: Ollama's /v1 surface locally, a
// hosted open-model API in the cloud. BaseURL and Client are fields rather
// than constants so tests can point at an httptest server.
type OpenAIResponder struct {
	BaseURL string       // e.g. http://ollama:11434/v1, no trailing slash
	Model   string       // e.g. llama3.2:3b
	APIKey  string       // empty for a local Ollama; sent as a Bearer token when set
	Client  *http.Client // injected; see newLLMClient for the production one
}

// newLLMClient sets no Client.Timeout — a long generation is normal, not a
// fault — but bounds the wait for response headers so a wedged provider fails
// instead of pinning a goroutine forever. A cold VRAM load measured 32.8s on a
// GTX 1060, so 120s leaves room without being unbounded.
func newLLMClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 120 * time.Second
	return &http.Client{Transport: transport}
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIRequest struct {
	Model         string              `json:"model"`
	Stream        bool                `json:"stream"`
	StreamOptions openAIStreamOptions `json:"stream_options"`
	Messages      []openAIMessage     `json:"messages"`
}

// openAIChunk is one SSE frame's JSON payload. The three interesting kinds
// arrive separately: a text delta, a frame carrying finish_reason with an empty
// delta, and a trailing usage-only frame with no choices at all. Usage is a
// pointer so a frame without it is distinguishable from one reporting zeros.
type openAIChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int32 `json:"prompt_tokens"`
		CompletionTokens int32 `json:"completion_tokens"`
	} `json:"usage"`
}

// sseDataPrefix marks a payload line in a server-sent-event stream. Other
// fields (event:, id:, retry:) and comment lines (:) are ignored.
const sseDataPrefix = "data: "

// sseDoneSentinel terminates an OpenAI-compatible stream. Unlike Ollama's
// native NDJSON there is no done flag on the final JSON object, so this
// literal is the only end-of-stream signal.
const sseDoneSentinel = "[DONE]"

// Stream accumulates Usage as frames land and returns it on every exit path,
// including errors: finish_reason and the token counts arrive in separate
// frames, so a stream that dies late has still reported real numbers. See the
// Usage doc comment in responder.go.
func (o *OpenAIResponder) Stream(ctx context.Context, req *chatpb.ChatRequest, emit func(delta string) error) (Usage, error) {
	var usage Usage

	body, err := json.Marshal(openAIRequest{
		Model:         o.Model,
		Stream:        true,
		StreamOptions: openAIStreamOptions{IncludeUsage: true},
		Messages:      toOpenAIMessages(req.GetMessages()),
	})
	if err != nil {
		return usage, status.Errorf(codes.Internal, "encoding completions request: %v", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return usage, status.Errorf(codes.Internal, "building completions request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		// Omitted entirely when unset: a local Ollama needs no credential, and
		// a bare "Bearer " would be a malformed one.
		httpReq.Header.Set("Authorization", "Bearer "+o.APIKey)
	}

	resp, err := o.Client.Do(httpReq)
	if err != nil {
		return usage, err
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	// Default 64KB per line is generous for a delta but not for a provider
	// that batches, so raise the ceiling rather than fail on a long frame.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	firstDelta := true
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return usage, err
		}

		payload, ok := strings.CutPrefix(scanner.Text(), sseDataPrefix)
		if !ok {
			continue // blank separator line, comment, or a non-data SSE field
		}
		if payload == sseDoneSentinel {
			return usage, nil
		}

		var chunk openAIChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return usage, status.Errorf(codes.Internal, "decoding completions frame: %v", err)
		}

		if chunk.Usage != nil {
			usage.InputTokens = chunk.Usage.PromptTokens
			usage.OutputTokens = chunk.Usage.CompletionTokens
		}

		for _, choice := range chunk.Choices {
			if choice.FinishReason != "" {
				usage.StopReason = choice.FinishReason
			}
			// The finish_reason and usage frames carry an empty delta;
			// emitting it would send a text_delta the client must ignore.
			if choice.Delta.Content == "" {
				continue
			}
			if firstDelta {
				firstDelta = false
			}
			if err := emit(choice.Delta.Content); err != nil {
				return usage, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return usage, status.Errorf(codes.Internal, "reading completions stream: %v", err)
	}

	// Ran out of frames without a [DONE]: the provider died mid-generation.
	return usage, status.Error(codes.Internal, "completions stream ended without a [DONE] sentinel")
}

// toOpenAIMessages maps proto roles to the wire format's role strings.
// ROLE_UNSPECIFIED has no mapping, which is why validateHistory rejects it
// before a request reaches here.
func toOpenAIMessages(msgs []*chatpb.Message) []openAIMessage {
	out := make([]openAIMessage, 0, len(msgs))
	for _, m := range msgs {
		role := "user"
		if m.GetRole() == chatpb.Role_ROLE_ASSISTANT {
			role = "assistant"
		}
		out = append(out, openAIMessage{Role: role, Content: m.GetContent()})
	}
	return out
}
```

`firstDelta` is set but not yet read — Task 5 hangs the time-to-first-token observation off it. The non-200 handling and the `reason` labels land in Task 4; this step deliberately returns raw errors for those paths.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd chat-service && go test ./... && go vet ./...`

Expected: PASS, no vet output. `newLLMClient` is unused until Task 6, which Go permits for a package-level func.

`TestOpenAIResponderKeepsUsageOnMidStreamFailure` passes via the `json.Unmarshal` error on the truncated frame, not the missing sentinel — either way `usage` comes back populated, which is what the test asserts.

- [ ] **Step 5: Commit**

```bash
git add chat-service/openai.go chat-service/openai_test.go
git commit -m "feat(chat): stream replies from an OpenAI-compatible provider"
```

---

### Task 4: Error taxonomy and `chat_provider_errors_total`

**Files:**
- Modify: `chat-service/metrics.go` (one new collector)
- Modify: `chat-service/openai.go` (route every failure through `providerError`)
- Test: `chat-service/openai_test.go` (append)

**Interfaces:**
- Consumes: `OpenAIResponder.Stream` from Task 3; `counterValue(c prometheus.Counter) float64` already defined in `chat_test.go:23`, same package; the real 404 status confirmed in Task 1 Step 5.
- Produces:
  - `var chatProviderErrorsTotal *prometheus.CounterVec` with label `reason`, values `unreachable | model_missing | auth_error | rate_limited | http_error | decode_error | config_error`.
  - `func providerError(reason string, code codes.Code, format string, args ...any) error`.
  - Test helper `hangingSSEServer(t *testing.T, frames ...string) *httptest.Server`.

- [ ] **Step 1: Write the failing tests**

Append to `chat-service/openai_test.go`:

```go
// hangingSSEServer streams the given frames and then blocks until the client
// goes away, so a test can cancel mid-stream while the body is still open.
func hangingSSEServer(t *testing.T, frames ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			io.WriteString(w, f+"\n\n")
			w.(http.Flusher).Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenAIResponderProviderErrors(t *testing.T) {
	tests := []struct {
		name       string
		baseURL    func(t *testing.T) string
		wantCode   codes.Code
		wantReason string
		wantInMsg  string
	}{
		{
			name: "provider not running",
			baseURL: func(t *testing.T) string {
				// A server closed before the request: the dial fails exactly
				// as it does when the ollama container is down.
				srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				url := srv.URL
				srv.Close()
				return url
			},
			wantCode:   codes.Unavailable,
			wantReason: "unreachable",
			wantInMsg:  "unreachable",
		},
		{
			name: "model not available",
			baseURL: func(t *testing.T) string {
				return sseServer(t, http.StatusNotFound).URL
			},
			wantCode:   codes.Unavailable,
			wantReason: "model_missing",
			// The most likely local misconfiguration, so the message names the fix.
			wantInMsg: "ollama pull llama3.2:3b",
		},
		{
			name: "bad api key",
			baseURL: func(t *testing.T) string {
				return sseServer(t, http.StatusUnauthorized).URL
			},
			// Internal, not Unauthenticated: the browser's credentials are not
			// the problem, our LLM_API_KEY is.
			wantCode:   codes.Internal,
			wantReason: "auth_error",
			wantInMsg:  "LLM_API_KEY",
		},
		{
			name: "rate limited",
			baseURL: func(t *testing.T) string {
				return sseServer(t, http.StatusTooManyRequests).URL
			},
			wantCode:   codes.Unavailable,
			wantReason: "rate_limited",
			wantInMsg:  "rate limit",
		},
		{
			name: "other non-200",
			baseURL: func(t *testing.T) string {
				return sseServer(t, http.StatusInternalServerError).URL
			},
			wantCode:   codes.Unavailable,
			wantReason: "http_error",
			wantInMsg:  "500",
		},
		{
			name: "malformed frame json",
			baseURL: func(t *testing.T) string {
				return sseServer(t, http.StatusOK, frameHel, `data: {"choices":[`).URL
			},
			wantCode:   codes.Internal,
			wantReason: "decode_error",
			wantInMsg:  "decoding completions frame",
		},
		{
			name: "stream ends without a done sentinel",
			baseURL: func(t *testing.T) string {
				return sseServer(t, http.StatusOK, frameHel, frameLo).URL
			},
			wantCode:   codes.Internal,
			wantReason: "decode_error",
			wantInMsg:  "without a [DONE] sentinel",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := counterValue(chatProviderErrorsTotal.WithLabelValues(tt.wantReason))

			r := newTestLLM(tt.baseURL(t))
			req := &chatpb.ChatRequest{Messages: userHistory("hi")}
			_, err := r.Stream(context.Background(), req, func(string) error { return nil })

			if status.Code(err) != tt.wantCode {
				t.Fatalf("Stream() code = %v, want %v (err = %v)", status.Code(err), tt.wantCode, err)
			}
			if !strings.Contains(err.Error(), tt.wantInMsg) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantInMsg)
			}
			if got := counterValue(chatProviderErrorsTotal.WithLabelValues(tt.wantReason)); got != before+1 {
				t.Errorf("chat_provider_errors_total{reason=%q} = %v, want %v", tt.wantReason, got, before+1)
			}
		})
	}
}

// A hosted provider explains itself in the response body. Losing that text
// turns a one-line fix into a debugging session, so it must reach the status.
func TestOpenAIResponderSurfacesProviderErrorText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"This model's maximum context length is 128000 tokens.","type":"invalid_request_error"}}`)
	}))
	t.Cleanup(srv.Close)

	r := newTestLLM(srv.URL)
	req := &chatpb.ChatRequest{Messages: userHistory("hi")}
	_, err := r.Stream(context.Background(), req, func(string) error { return nil })

	if status.Code(err) != codes.Unavailable {
		t.Fatalf("Stream() code = %v, want Unavailable (err = %v)", status.Code(err), err)
	}
	if !strings.Contains(err.Error(), "maximum context length") {
		t.Errorf("error = %q, want it to quote the provider's message", err.Error())
	}
}

// A body that is not OpenAI-shaped still has to come through — Ollama returns
// plain text, and truncating to nothing would be worse than passing it along.
func TestOpenAIResponderSurfacesNonJSONErrorText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "llama runner process has terminated\n")
	}))
	t.Cleanup(srv.Close)

	r := newTestLLM(srv.URL)
	req := &chatpb.ChatRequest{Messages: userHistory("hi")}
	_, err := r.Stream(context.Background(), req, func(string) error { return nil })

	if !strings.Contains(err.Error(), "llama runner process has terminated") {
		t.Errorf("error = %q, want it to include the raw body", err.Error())
	}
}

func TestOpenAIResponderStopsOnContextCancel(t *testing.T) {
	srv := hangingSSEServer(t, frameHel, frameLo, frameFinish, frameDone)
	r := newTestLLM(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A hung-up browser is not a provider fault, so no reason label may move.
	beforeUnreachable := counterValue(chatProviderErrorsTotal.WithLabelValues("unreachable"))
	beforeDecode := counterValue(chatProviderErrorsTotal.WithLabelValues("decode_error"))

	count := 0
	req := &chatpb.ChatRequest{Messages: userHistory("hi")}
	_, err := r.Stream(ctx, req, func(string) error {
		count++
		cancel()
		return nil
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Stream() error = %v, want context.Canceled unwrapped so classifyOutcome sees it", err)
	}
	if count != 1 {
		t.Errorf("emitted %d deltas, want 1 — the loop must check ctx before handling the next frame", count)
	}
	if got := counterValue(chatProviderErrorsTotal.WithLabelValues("unreachable")); got != beforeUnreachable {
		t.Errorf("unreachable counter moved on cancel: %v, want %v", got, beforeUnreachable)
	}
	if got := counterValue(chatProviderErrorsTotal.WithLabelValues("decode_error")); got != beforeDecode {
		t.Errorf("decode_error counter moved on cancel: %v, want %v", got, beforeDecode)
	}
}
```

Extend `openai_test.go`'s import block with `"strings"`, `"google.golang.org/grpc/codes"`, and `"google.golang.org/grpc/status"`.

If Task 1 Step 5 found the compat layer returns something other than 404 for a missing model, change `http.StatusNotFound` here and the status check in Step 3 to the real status.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd chat-service && go test -run TestOpenAIResponder ./...`

Expected: FAIL to compile with `undefined: chatProviderErrorsTotal`.

- [ ] **Step 3: Write minimal implementation**

3a. In `chat-service/metrics.go`, add inside the existing `var (...)` block:

```go
	chatProviderErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chat_provider_errors_total",
		Help: "LLM provider failures by reason. chat_streams_total{status=\"error\"} counts the same failures; this breaks down the cause.",
	}, []string{"reason"})
```

3b. In `chat-service/openai.go`, add the helper:

```go
// providerError records why a provider call failed and returns the gRPC status
// the handler passes through unchanged. Every provider failure path goes
// through here so the counter and the status code cannot drift apart.
//
// Client cancellation must NOT come through here: it is expected, not a fault,
// and chat.go's classifyOutcome needs the bare context error.
func providerError(reason string, code codes.Code, format string, args ...any) error {
	chatProviderErrorsTotal.WithLabelValues(reason).Inc()
	return status.Errorf(code, format, args...)
}
```

3c. Replace the raw error returns in `Stream`. The marshal and request-build returns become:

```go
	if err != nil {
		return usage, providerError("config_error", codes.Internal, "encoding completions request: %v", err)
	}
```

```go
	if err != nil {
		return usage, providerError("config_error", codes.Internal, "building completions request: %v", err)
	}
```

The `Client.Do` return becomes:

```go
	resp, err := o.Client.Do(httpReq)
	if err != nil {
		// A cancelled request surfaces here as a transport error wrapping
		// ctx.Err(); return the bare context error so the handler classifies
		// it as cancelled rather than as a dead provider.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return usage, ctxErr
		}
		return usage, providerError("unreachable", codes.Unavailable,
			"llm provider unreachable at %s: %v", o.BaseURL, err)
	}
	defer resp.Body.Close()
```

Immediately after `defer resp.Body.Close()`, add the status handling. Every branch quotes the provider's own explanation, because `HTTP 400` alone costs real debugging time against a hosted API:

```go
	if resp.StatusCode != http.StatusOK {
		detail := readErrorBody(resp.Body)
		switch {
		case resp.StatusCode == http.StatusNotFound:
			// A local Ollama 404s on an unpulled model, which is the most
			// likely misconfiguration here; a hosted provider 404s on a model
			// name it does not serve. One message covers both.
			return usage, providerError("model_missing", codes.Unavailable,
				"model %q not available at %s: %s (for a local Ollama, run: ollama pull %s)",
				o.Model, o.BaseURL, detail, o.Model)
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			// codes.Internal, not Unauthenticated: the browser's credentials
			// are not at fault, our configuration is.
			return usage, providerError("auth_error", codes.Internal,
				"llm provider rejected our credentials (HTTP %d): %s; check LLM_API_KEY", resp.StatusCode, detail)
		case resp.StatusCode == http.StatusTooManyRequests:
			// Both a transient rate limit and a hard quota exhaustion arrive as
			// 429; detail is what tells them apart.
			return usage, providerError("rate_limited", codes.Unavailable,
				"llm provider rate limit hit (HTTP 429): %s", detail)
		default:
			return usage, providerError("http_error", codes.Unavailable,
				"llm provider returned HTTP %d: %s", resp.StatusCode, detail)
		}
	}
```

and the helper it uses:

```go
// readErrorBody summarises a provider's error response for the status message.
// Bounded, because an error page can be arbitrarily large and this text ends up
// in a gRPC status the browser receives. OpenAI-shaped bodies get their
// message field lifted out; anything else is flattened to one line.
func readErrorBody(r io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(r, 2048))
	if err != nil || len(raw) == 0 {
		return "no error body"
	}
	var wrapper struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &wrapper) == nil && wrapper.Error.Message != "" {
		return wrapper.Error.Message
	}
	return strings.TrimSpace(strings.ReplaceAll(string(raw), "\n", " "))
}
```

Add `"io"` to `openai.go`'s import block.

The frame-decode failure becomes:

```go
		var chunk openAIChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return usage, providerError("decode_error", codes.Internal,
				"decoding completions frame: %v", err)
		}
```

The scanner error and the missing-sentinel return become:

```go
	if err := scanner.Err(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return usage, ctxErr
		}
		return usage, providerError("decode_error", codes.Internal, "reading completions stream: %v", err)
	}

	return usage, providerError("decode_error", codes.Internal,
		"completions stream ended without a [DONE] sentinel")
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd chat-service && go test ./... && go vet ./...`

Expected: PASS, no vet output.

- [ ] **Step 5: Commit**

```bash
git add chat-service/openai.go chat-service/openai_test.go chat-service/metrics.go
git commit -m "feat(chat): classify LLM provider failures and count them by reason"
```

---

### Task 5: Token counts and time-to-first-token metrics

Two collectors, two owners: `chat.go` records tokens because that is where `Usage` already lands, preserving the handler's single-metrics-site discipline for its own two metrics; the responder records time-to-first-token because it is the only place that knows when the first delta arrived.

**Files:**
- Modify: `chat-service/metrics.go` (two collectors plus `recordTokens`)
- Modify: `chat-service/chat.go:50-75` (call `recordTokens` once, before the error branch)
- Modify: `chat-service/openai.go` (observe TTFT on the first non-empty delta)
- Test: `chat-service/openai_test.go` (append), `chat-service/chat_test.go` (append)

**Interfaces:**
- Consumes: `Usage` from `responder.go`; `OpenAIResponder.Stream` from Tasks 3-4; `counterValue` from `chat_test.go`. Note `erroringResponder` (`chat_test.go:84`) cannot be reused here — it always reports zero usage, so this task adds `usageResponder` instead.
- Produces:
  - `var chatTokensTotal *prometheus.CounterVec` with label `direction`, values `input | output`.
  - `var chatTimeToFirstTokenSeconds prometheus.Histogram`.
  - `func recordTokens(u Usage)`.
  - Test helper `histogramCount(h prometheus.Histogram) uint64`.

- [ ] **Step 1: Write the failing tests**

Append to `chat-service/openai_test.go`:

```go
// histogramCount reads a histogram's sample count directly, for the same
// reason counterValue exists: the testutil subpackage needs go.sum entries
// this repo has not resolved.
func histogramCount(h prometheus.Histogram) uint64 {
	var m dto.Metric
	if err := h.Write(&m); err != nil {
		return 0
	}
	return m.GetHistogram().GetSampleCount()
}

func TestOpenAIResponderObservesTimeToFirstToken(t *testing.T) {
	srv := sseServer(t, http.StatusOK, frameHel, frameLo, frameFinish, frameUsage, frameDone)
	r := newTestLLM(srv.URL)

	before := histogramCount(chatTimeToFirstTokenSeconds)
	req := &chatpb.ChatRequest{Messages: userHistory("hi")}
	if _, err := r.Stream(context.Background(), req, func(string) error { return nil }); err != nil {
		t.Fatalf("Stream() error = %v, want nil", err)
	}

	// Exactly one observation per stream, on the first delta — not per frame.
	if got := histogramCount(chatTimeToFirstTokenSeconds); got != before+1 {
		t.Errorf("TTFT sample count = %d, want %d", got, before+1)
	}
}

func TestOpenAIResponderSkipsTimeToFirstTokenWhenNoDeltas(t *testing.T) {
	srv := sseServer(t, http.StatusOK, frameFinish, frameUsage, frameDone)
	r := newTestLLM(srv.URL)

	before := histogramCount(chatTimeToFirstTokenSeconds)
	req := &chatpb.ChatRequest{Messages: userHistory("hi")}
	if _, err := r.Stream(context.Background(), req, func(string) error { return nil }); err != nil {
		t.Fatalf("Stream() error = %v, want nil", err)
	}

	if got := histogramCount(chatTimeToFirstTokenSeconds); got != before {
		t.Errorf("TTFT sample count = %d, want %d — an empty reply has no first token", got, before)
	}
}
```

Extend `openai_test.go`'s imports with `"github.com/prometheus/client_golang/prometheus"` and `dto "github.com/prometheus/client_model/go"`.

Append to `chat-service/chat_test.go`:

```go
// usageResponder returns a fixed Usage and error without streaming anything,
// so a test can drive the handler's accounting directly. erroringResponder
// cannot: it always reports zero usage.
type usageResponder struct {
	usage Usage
	err   error
}

func (r *usageResponder) Stream(ctx context.Context, req *chatpb.ChatRequest, emit func(delta string) error) (Usage, error) {
	return r.usage, r.err
}

func TestChatRecordsTokenCounts(t *testing.T) {
	beforeIn := counterValue(chatTokensTotal.WithLabelValues("input"))
	beforeOut := counterValue(chatTokensTotal.WithLabelValues("output"))

	srv := &chatServer{responder: &usageResponder{usage: Usage{
		StopReason:   "stop",
		InputTokens:  26,
		OutputTokens: 298,
	}}}
	if err := srv.Chat(&chatpb.ChatRequest{Messages: userHistory("hi")}, newFakeStream(context.Background())); err != nil {
		t.Fatalf("Chat() error = %v, want nil", err)
	}

	if got := counterValue(chatTokensTotal.WithLabelValues("input")); got != beforeIn+26 {
		t.Errorf("chat_tokens_total{direction=\"input\"} = %v, want %v", got, beforeIn+26)
	}
	if got := counterValue(chatTokensTotal.WithLabelValues("output")); got != beforeOut+298 {
		t.Errorf("chat_tokens_total{direction=\"output\"} = %v, want %v", got, beforeOut+298)
	}
}

func TestChatRecordsPartialTokenCountsOnError(t *testing.T) {
	beforeOut := counterValue(chatTokensTotal.WithLabelValues("output"))

	// A provider that fails after reporting usage still consumed those tokens,
	// so the counter must move even though Chat returns an error.
	srv := &chatServer{responder: &usageResponder{
		usage: Usage{OutputTokens: 400},
		err:   errors.New("provider exploded"),
	}}
	err := srv.Chat(&chatpb.ChatRequest{Messages: userHistory("hi")}, newFakeStream(context.Background()))
	if err == nil {
		t.Fatal("Chat() error = nil, want the responder error")
	}

	if got := counterValue(chatTokensTotal.WithLabelValues("output")); got != beforeOut+400 {
		t.Errorf("chat_tokens_total{direction=\"output\"} = %v, want %v", got, beforeOut+400)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd chat-service && go test -run 'TestOpenAIResponderObserves|TestOpenAIResponderSkips|TestChatRecords' ./...`

Expected: FAIL to compile with `undefined: chatTimeToFirstTokenSeconds` and `undefined: chatTokensTotal`.

- [ ] **Step 3: Write minimal implementation**

3a. In `chat-service/metrics.go`, add to the `var (...)` block:

```go
	chatTokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chat_tokens_total",
		Help: "Tokens reported by the responder, by direction.",
	}, []string{"direction"})

	chatTimeToFirstTokenSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "chat_time_to_first_token_seconds",
		Help: "Latency from the provider request to the first streamed delta.",
		// DefBuckets stop at 10s, which would dump every cold VRAM load into
		// +Inf and hide the difference between a warm reply (~1s) and a model
		// load (~33s on a GTX 1060).
		Buckets: []float64{.1, .25, .5, 1, 2, 5, 10, 30, 60, 120},
	})
```

Then add below the `var` block:

```go
// recordTokens accounts one turn's usage. Zero values are skipped so a
// responder that reports no tokens — the echo stub, or a provider that ignores
// stream_options.include_usage — leaves the series alone rather than pinning
// it at zero.
func recordTokens(u Usage) {
	if u.InputTokens > 0 {
		chatTokensTotal.WithLabelValues("input").Add(float64(u.InputTokens))
	}
	if u.OutputTokens > 0 {
		chatTokensTotal.WithLabelValues("output").Add(float64(u.OutputTokens))
	}
}
```

3b. In `chat-service/chat.go`, insert one call between the `s.responder.Stream(...)` call and the `if err != nil {` that follows it:

```go
	// Recorded before branching on err: usage may be partially populated on a
	// mid-stream failure (see the Usage doc comment), and those tokens were
	// still consumed. This is chat_tokens_total, not chat_streams_total — the
	// single-metrics-site rule in this function's doc comment is unaffected.
	recordTokens(usage)
```

3c. In `chat-service/openai.go`, capture the start time and observe on the first delta. Add immediately before `resp, err := o.Client.Do(httpReq)`:

```go
	start := time.Now()
```

Then replace Task 3's `firstDelta` branch — which currently only flips the flag — with the observing version:

```go
			if firstDelta {
				chatTimeToFirstTokenSeconds.Observe(time.Since(start).Seconds())
				firstDelta = false
			}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd chat-service && go test ./... && go vet ./...`

Expected: PASS, no vet output.

- [ ] **Step 5: Commit**

```bash
git add chat-service/metrics.go chat-service/chat.go chat-service/openai.go chat-service/openai_test.go chat-service/chat_test.go
git commit -m "feat(chat): record token counts and time to first token"
```

---

### Task 6: Wire the responder up through config

**Files:**
- Modify: `chat-service/main.go:19-74`
- Modify: `chat-service/.env.example`
- Modify: `chat-service/.env` (untracked, local only — not committed)
- Modify: `docker-compose.yml` (`chat-service` block only — `depends_on`)
- Test: `chat-service/main_test.go` (create)

**Interfaces:**
- Consumes: `OpenAIResponder`, `defaultLLMBaseURL`, `defaultLLMModel`, `newLLMClient` from Task 3; `EchoResponder` and `echoDelay` already in the package; the `ollama` service from Task 1.
- Produces: `func newResponder() (Responder, error)` and `func envOr(key, fallback string) string`.

- [ ] **Step 1: Write the failing test**

Create `chat-service/main_test.go`:

```go
package main

import (
	"testing"
)

func TestNewResponder(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
		check   func(t *testing.T, r Responder)
	}{
		{
			name: "defaults to echo so an empty .env still boots",
			// Set explicitly empty rather than left unset: envOr treats an
			// empty value as absent, and this keeps the case honest on a
			// machine that exports RESPONDER in its shell.
			env: map[string]string{"RESPONDER": ""},
			check: func(t *testing.T, r Responder) {
				if _, ok := r.(*EchoResponder); !ok {
					t.Errorf("responder = %T, want *EchoResponder", r)
				}
			},
		},
		{
			name: "llm uses the documented defaults",
			env:  map[string]string{"RESPONDER": "llm"},
			check: func(t *testing.T, r Responder) {
				o, ok := r.(*OpenAIResponder)
				if !ok {
					t.Fatalf("responder = %T, want *OpenAIResponder", r)
				}
				if o.BaseURL != defaultLLMBaseURL {
					t.Errorf("BaseURL = %q, want %q", o.BaseURL, defaultLLMBaseURL)
				}
				if o.Model != defaultLLMModel {
					t.Errorf("Model = %q, want %q", o.Model, defaultLLMModel)
				}
				if o.APIKey != "" {
					t.Errorf("APIKey = %q, want empty — a local Ollama needs no credential", o.APIKey)
				}
				if o.Client == nil {
					t.Error("Client = nil, want a client with a response-header timeout")
				}
			},
		},
		{
			name: "llm honours base url, model and api key",
			env: map[string]string{
				"RESPONDER":    "llm",
				"LLM_BASE_URL": "https://api.groq.com/openai/v1/",
				"LLM_MODEL":    "llama-3.3-70b-versatile",
				"LLM_API_KEY":  "gsk_secret",
			},
			check: func(t *testing.T, r Responder) {
				o := r.(*OpenAIResponder)
				// The trailing slash is trimmed, or request paths become
				// //chat/completions.
				if o.BaseURL != "https://api.groq.com/openai/v1" {
					t.Errorf("BaseURL = %q, want %q", o.BaseURL, "https://api.groq.com/openai/v1")
				}
				if o.Model != "llama-3.3-70b-versatile" {
					t.Errorf("Model = %q, want %q", o.Model, "llama-3.3-70b-versatile")
				}
				if o.APIKey != "gsk_secret" {
					t.Errorf("APIKey = %q, want %q", o.APIKey, "gsk_secret")
				}
			},
		},
		{
			// A silent fallback to echo would make a misconfigured demo look
			// like a working model.
			name:    "an unknown RESPONDER is a startup error, never a fallback",
			env:     map[string]string{"RESPONDER": "ollama"},
			wantErr: true,
		},
		{
			name:    "a malformed LLM_BASE_URL fails at startup",
			env:     map[string]string{"RESPONDER": "llm", "LLM_BASE_URL": "http://[::1"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			r, err := newResponder()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("newResponder() error = nil, want an error (got %T)", r)
				}
				return
			}
			if err != nil {
				t.Fatalf("newResponder() error = %v, want nil", err)
			}
			tt.check(t, r)
		})
	}
}
```

`t.Setenv` fails a test that has called `t.Parallel`, so these subtests must stay serial. Do not add `t.Parallel()`.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd chat-service && go test -run TestNewResponder ./...`

Expected: FAIL to compile with `undefined: newResponder`.

- [ ] **Step 3: Write the implementation**

3a. In `chat-service/main.go`, replace the `RegisterChatServiceServer` call at lines 56-58:

```go
	chatpb.RegisterChatServiceServer(grpcServer, &chatServer{
		responder: &EchoResponder{Delay: echoDelay()},
	})
```

with:

```go
	responder, err := newResponder()
	if err != nil {
		slog.Error("failed to build responder", "error", err)
		os.Exit(1)
	}
	chatpb.RegisterChatServiceServer(grpcServer, &chatServer{responder: responder})
```

3b. Add to `chat-service/main.go`, after `echoDelay`:

```go
// newResponder builds the Responder named by RESPONDER, defaulting to the echo
// stub so ghz load tests and CI run with no model and no GPU. An unrecognised
// name is a startup error rather than a silent fallback: a demo that quietly
// answers with echo looks like a working model.
func newResponder() (Responder, error) {
	switch name := envOr("RESPONDER", "echo"); name {
	case "echo":
		slog.Info("responder configured", "responder", "echo")
		return &EchoResponder{Delay: echoDelay()}, nil

	case "llm":
		baseURL := envOr("LLM_BASE_URL", defaultLLMBaseURL)
		if _, err := url.Parse(baseURL); err != nil {
			return nil, fmt.Errorf("LLM_BASE_URL %q is not a valid URL: %w", baseURL, err)
		}
		model := envOr("LLM_MODEL", defaultLLMModel)
		apiKey := os.Getenv("LLM_API_KEY")
		// The key itself is never logged; whether one is set is worth knowing
		// when a provider starts returning 401.
		slog.Info("responder configured", "responder", "llm",
			"base_url", baseURL, "model", model, "api_key_set", apiKey != "")
		return &OpenAIResponder{
			// Trimmed because the request path is appended directly.
			BaseURL: strings.TrimSuffix(baseURL, "/"),
			Model:   model,
			APIKey:  apiKey,
			Client:  newLLMClient(),
		}, nil

	default:
		return nil, fmt.Errorf("unknown RESPONDER %q, want echo or llm", name)
	}
}

// envOr treats an empty variable as unset, so a commented-out or blank line in
// .env falls back to the default instead of producing an empty model name.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
```

Add `"fmt"`, `"net/url"`, and `"strings"` to `main.go`'s import block.

3c. Append to `chat-service/.env.example`:

```
# Responder backing the Chat RPC: echo | llm. An unrecognised value is a
# startup error, not a fallback. echo needs no model, so load tests and CI stay
# zero-cost.
RESPONDER=echo
# Any OpenAI-compatible provider. Locally this is the ollama compose service;
# in cloud, swap the URL and set LLM_API_KEY — no code change.
#   https://api.groq.com/openai/v1
LLM_BASE_URL=http://ollama:11434/v1
# NOTE: duplicated in docker-compose.yml, where the ollama service pulls this
# model and healthchecks on it. Change both together.
LLM_MODEL=llama3.2:3b
# Empty for a local Ollama, which needs no credential. When set, it is sent as
# an Authorization: Bearer header. Never commit a real key.
LLM_API_KEY=
```

3d. Append the same four variables to the local, untracked `chat-service/.env`, with `RESPONDER=llm` so the stack uses the real model. Compose reads this file via `env_file`, so an inline variable on the `docker compose` command line will not reach the container.

3e. In `docker-compose.yml`, add `depends_on` to the `chat-service` service:

```yaml
    depends_on:
      ollama:
        condition: service_healthy
```

`chat-service` would survive without it — a down provider yields `Unavailable`, which the frontend already handles — but waiting means the first message after `docker compose up` gets a real reply instead of an error while the model is still downloading.

- [ ] **Step 4: Verify**

Run:

```bash
cd chat-service && go build ./... && go vet ./... && go test ./...
cd .. && docker compose config >/dev/null && docker compose up -d --build chat-service
docker compose logs chat-service | grep "responder configured"
```

Expected: build, vet and tests pass; `docker compose config` reports no error; the log line reads `"responder":"llm"` with `"api_key_set":false` and the `ollama` base URL.

- [ ] **Step 5: Commit**

`.env` is untracked and must not be added.

```bash
git add chat-service/main.go chat-service/main_test.go chat-service/.env.example docker-compose.yml
git commit -m "feat(chat): select the responder with RESPONDER, defaulting to echo"
```

---

### Task 7: End-to-end verification

Tests are hermetic by design, so nothing so far has proved the browser gets a real reply. This task writes no code.

**Files:**
- Modify: `chat-service/.env` (untracked, local only)

**Interfaces:**
- Consumes: everything from Tasks 1-6.
- Produces: nothing. Record the observed numbers in the completion report.

- [ ] **Step 1: Bring the whole stack up**

```bash
docker compose up -d --build
docker compose ps
```

Expected: every service up, `ollama` healthy.

- [ ] **Step 2: Verify a real streaming reply**

Open the frontend and send a message.

Expected: the reply renders progressively and reads as a genuine model answer rather than an echo of the input. Stop halts it mid-stream. The first request after a `OLLAMA_KEEP_ALIVE` expiry takes roughly 33s to the first token on a GTX 1060.

- [ ] **Step 3: Verify the metrics**

`chat-service` publishes no host port, so read them from inside:

```bash
docker compose exec chat-service wget -qO- http://localhost:9091/metrics | grep chat_
```

Expected: `chat_streams_total{status="ok"}` incremented; `chat_time_to_first_token_seconds_count` at least 1, with a cold start landing in the 30s+ bucket rather than `+Inf`.

For `chat_tokens_total`: non-zero for both directions **if** Task 1 Step 4 confirmed the usage frame. If it did not, both series are absent — expected, not a bug. Say which case holds.

- [ ] **Step 4: Verify the unreachable path**

```bash
docker compose stop ollama
```

Send a message in the frontend.

Expected: the UI shows an error, rolls the turn back, and restores the typed text. `chat_provider_errors_total{reason="unreachable"}` and `chat_streams_total{status="error"}` each increment by 1. Then `docker compose start ollama`.

- [ ] **Step 5: Verify the model_missing path**

Set `LLM_MODEL=nope` in `chat-service/.env`, run `docker compose up -d chat-service`, and send a message.

Expected: `chat_provider_errors_total{reason="model_missing"}` increments, and the `chat-service` log carries the error naming the fix — `model "nope" not available at http://ollama:11434/v1; for a local Ollama run: ollama pull nope`.

- [ ] **Step 6: Verify the cancel path is not counted as an error**

Restore `LLM_MODEL=llama3.2:3b`, `docker compose up -d chat-service`, send a long prompt and hit Stop mid-reply.

Expected: `chat_streams_total{status="cancelled"}` increments, and **no** `chat_provider_errors_total` series moves.

- [ ] **Step 7: Confirm traces still cross the new hop**

Open Grafana at `:3000`, find the chat trace in Tempo.

Expected: the span tree is intact with `chat.history_len` set. The outbound HTTP call to Ollama will **not** appear as a child span — this plan adds no `otelhttp` transport. Note that as a follow-up rather than a defect; it is out of scope here.

- [ ] **Step 8: Report**

No commit — `.env` is untracked and nothing else changed. Report the observed cold and warm time-to-first-token, whether token counts materialised, the eval rate from Task 1 Step 3, and any step whose expectation did not hold.

---

## Out of scope

Per the spec and its amendment: system prompts, model parameters in `ChatRequest` (temperature, max tokens), Grafana panels for the three new metrics, conversation persistence, auth, and the actual cloud deployment. Two things this plan surfaces but deliberately leaves alone: wrapping the provider call in an `otelhttp` transport so the HTTP hop appears in Tempo, and scraping anything from the `ollama` container, which exposes no Prometheus endpoint. Both are follow-ups.

## Blocker for the cloud step, not for this plan

This plan is safe to execute as written: Ollama is local, unmetered, and reachable only from the compose network, so the worst case is a wasted GPU cycle.

The moment `LLM_BASE_URL` points at a metered provider such as OpenAI, that stops being true. The `Chat` RPC is unauthenticated by design — `chat.go`'s history limits exist precisely because "a caller that skips the frontend can send anything" — so a public deployment would let anyone spend the API key at will. `maxHistoryBytes` caps input at 32KB per turn but nothing caps the number of turns or the output length.

Do not deploy against a paid provider until at least these three exist:
- a spend cap at the provider (OpenAI supports hard monthly limits — set one),
- a `max_tokens` ceiling on each request, which needs the `ChatRequest` model-parameters work this plan lists as out of scope,
- rate limiting per client at Envoy or above.

`chat_tokens_total` becomes the cost signal at that point, which is a reason to confirm in Task 1 Step 4 that the usage frame really arrives.
