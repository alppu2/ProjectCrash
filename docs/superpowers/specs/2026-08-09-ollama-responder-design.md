# Ollama-Backed Responder — Design

**Date:** 2026-08-09
**Status:** Approved, not yet implemented

Replace `chat-service`'s `EchoResponder` with one that calls a locally hosted
model through Ollama, so the chat pipeline produces real replies at zero API
cost. Roadmap step 3 in `docs/scalability-learning-plan.md` names Claude as the
eventual provider; this lands the same shape first without a key, a bill, or a
network dependency.

Prerequisite: the chat-streaming slice
(`2026-08-07-chat-streaming-design.md`), which shipped the `Responder` seam
this design fills.

## Decisions

| Question | Choice | Why |
|---|---|---|
| Where Ollama runs | Natively on the Windows host, reached at `host.docker.internal:11434` | GPU acceleration works out of the box; a compose service would need NVIDIA Container Toolkit and would pin the compose file to machines with an NVIDIA card. Cost: `docker compose up` alone no longer brings up a working chat — Ollama must be installed separately. |
| Default model | `llama3.2:3b` | ~2GB of the 6GB on a GTX 1060, leaving headroom for context. Measured ~20 tok/s warm, so streaming reads as live. `qwen2.5:7b` fits but at ~4.7GB it spills to CPU as history grows. |
| Client | Ollama's native `/api/chat`, stdlib `net/http` + `encoding/json` | Zero new dependencies for one POST and a decode loop. Reports real token counts. The OpenAI-compatible endpoint would ease a swap to Groq or OpenRouter, but Anthropic's Messages API is not OpenAI-compatible, and Claude is the named endgame. |
| Responder selection | `RESPONDER` env var, default `echo` | Keeps `ghz` load tests and CI zero-cost and dependency-free. The same binary works with or without Ollama running. An unrecognised value is a startup error, never a silent fallback. |
| Provider unavailable | Fail the RPC with `codes.Unavailable` | The frontend already shows the error, rolls the turn back, and restores the typed text. Falling back to echo would make a degraded reply indistinguishable from a real one. |
| Structure | One `ollama.go`, injectable `BaseURL` and `*http.Client` | Injection is what makes it testable; splitting a client type out of ~130 lines adds files without adding testability. Extract when roadmap step 4 adds a second Ollama call site (embeddings). |

## Architecture

```
browser ──grpc-web──> Envoy :8080 ──grpc──> chat-service ──HTTP/NDJSON──> Ollama
          (unchanged)   (unchanged)          Chat handler                 host :11434
                                                  │                       (native, not compose)
                                                  └── Responder ◄── the only thing that changes
```

`proto/chat.proto`, `chat.go`'s RPC handler, `envoy.yaml`, and every frontend
file stay as they are. The `Responder` interface already takes the whole
`*chatpb.ChatRequest`, so no interface change is needed.

### Changes

| File | Change |
|---|---|
| `chat-service/ollama.go` | New. `OllamaResponder`. |
| `chat-service/ollama_test.go` | New. Table-driven tests against `httptest`. |
| `chat-service/main.go` | `newResponder() (Responder, error)` replacing the inline `&EchoResponder{...}`. |
| `chat-service/chat.go` | `validateHistory` rejects `ROLE_UNSPECIFIED`; record token counts from `Usage`. |
| `chat-service/metrics.go` | Three new metrics. |
| `chat-service/.env.example`, `.env` | `RESPONDER`, `OLLAMA_URL`, `OLLAMA_MODEL`. |
| `docker-compose.yml` | `extra_hosts: ["host.docker.internal:host-gateway"]` on `chat-service`. |

`extra_hosts` is a no-op on Docker Desktop, where the name already resolves. It
is there so the file still works on Linux Docker.

### Configuration

Every value has an in-code default, so an empty `.env` still boots as echo.

```
RESPONDER=ollama                              # echo | ollama, default echo
OLLAMA_URL=http://host.docker.internal:11434  # default
OLLAMA_MODEL=llama3.2:3b                      # default
```

## OllamaResponder

```go
type OllamaResponder struct {
	BaseURL string       // http://host.docker.internal:11434
	Model   string       // llama3.2:3b
	Client  *http.Client // injected so tests point at httptest
}
```

### Request

`POST {BaseURL}/api/chat`, built with `http.NewRequestWithContext(ctx, ...)`:

```json
{ "model": "llama3.2:3b", "stream": true,
  "messages": [ {"role": "user", "content": "..."} ] }
```

Roles map `ROLE_USER` → `"user"` and `ROLE_ASSISTANT` → `"assistant"`.
`ROLE_UNSPECIFIED` has no mapping, which is why `validateHistory` must reject
it — see below.

### Response

NDJSON, one object per line. `json.Decoder` reads concatenated objects
natively, so no manual line splitting. Verified against Ollama 0.32.6:

```json
{"message":{"role":"assistant","content":"Hel"},"done":false}
{"message":{"role":"assistant","content":"lo"},"done":false}
{"message":{"role":"assistant","content":""},"done":true,"done_reason":"stop",
 "prompt_eval_count":26,"eval_count":298}
```

Loop: decode, `emit(content)` when non-empty, break on `done`. Deltas travel
verbatim — Ollama already includes leading spaces, so unlike `EchoResponder`
there is no separator to reconstruct.

The final frame fills `Usage`: `done_reason` → `StopReason`,
`prompt_eval_count` → `InputTokens`, `eval_count` → `OutputTokens`.

### Usage on the error path

Token counts arrive **only** in the final frame. A mid-stream failure means
Ollama has reported nothing about tokens consumed, so `Stream` returns an empty
`Usage` alongside the error.

This is a provider limitation, not a pattern to copy. The `Usage` doc comment
states that callers must not discard it merely because `err` is non-nil, and a
Claude-backed responder *can* report partial usage. `ollama.go` will say so in
a comment, so the next implementation does not inherit this shape by imitation.

### Cancellation

Context cancellation closes the response body mid-decode. The resulting error
reaches the handler, where the existing `classifyOutcome` maps it to
`cancelled`. No new handling.

### Timeouts

No `Client.Timeout` — generations run long by design. `Transport` sets
`ResponseHeaderTimeout: 120s` so a wedged Ollama fails instead of pinning a
goroutine forever. A cold VRAM load measured 32.8s on this hardware, so the
ceiling has room without being unbounded.

## Error handling

| Condition | Detected as | gRPC code | `reason` label |
|---|---|---|---|
| Ollama not running | dial error on POST | `Unavailable` | `unreachable` |
| Model not pulled | HTTP 404 from `/api/chat` | `Unavailable` | `model_missing` |
| Other non-200 | status code | `Unavailable` | `http_error` |
| Malformed NDJSON | decode error mid-stream | `Internal` | `decode_error` |
| Client hung up | `ctx.Err()` | existing path | not counted |

The `model_missing` message names the fix:
`model "llama3.2:3b" not pulled; run: ollama pull llama3.2:3b`. A generic
"unavailable" would cost debugging time on the most likely misconfiguration.

## Metrics

```go
chatTokensTotal             CounterVec{"direction"}  // input | output
chatProviderErrorsTotal     CounterVec{"reason"}     // table above
chatTimeToFirstTokenSeconds Histogram
```

Buckets for time-to-first-token: `.1, .25, .5, 1, 2, 5, 10, 30, 60, 120`. The
Prometheus defaults stop at 10s, which would dump every cold start into `+Inf`
and hide the distinction between a warm reply and a VRAM load.

Token counts are recorded in `chat.go`, where `Usage` already lands, preserving
the handler's single-metrics-site invariant. Time-to-first-token is measured
inside the responder, which is the only place that knows when the first delta
arrived.

`chat_streams_total{status}` keeps its current meaning. A provider failure
appears there as `status="error"` and carries its specific cause in
`chat_provider_errors_total`.

## Testing

`ollama_test.go` is table-driven against `httptest.NewServer` serving canned
NDJSON, matching the helper-plus-subtests idiom in `responder_test.go`. No test
touches a real Ollama: CI has no GPU and no 2GB model, and `go test ./...` must
stay hermetic and fast. The injectable `BaseURL` and `Client` exist for this.

| Test | Asserts |
|---|---|
| streams deltas in order | `emit` called per `message.content`, verbatim |
| populates Usage from final frame | all three fields mapped |
| skips empty content frames | the `done:true` frame carries `content:""` |
| propagates emit error unchanged | interface contract; loop stops |
| stops on context cancel | returns `ctx.Err()`, no further emits |
| 404 → model_missing | message names the model and the `ollama pull` fix |
| non-200 → http_error | |
| malformed NDJSON → decode_error | truncated object mid-stream |
| dial failure → unreachable | server closed before the request |

One case joins `chat_test.go` beside `TestValidateHistoryBounds`:
`validateHistory` rejects `ROLE_UNSPECIFIED`.

### Manual verification

Tests cannot cover the real provider. Against a running Ollama:

1. Set `RESPONDER=ollama` in `chat-service/.env` (compose passes env through
   `env_file`, so an inline variable on the `docker compose` command line will
   not reach the container), then
   `docker compose up -d --build chat-service`.
2. Send a message in the browser: the reply renders word by word, and Stop
   halts it mid-stream.
3. The first request after an idle unload takes ~33s to first token and lands
   in the 30s+ bucket.
4. `curl localhost:9091/metrics | grep chat_` shows non-zero
   `chat_tokens_total` for both directions and a TTFT observation.
5. Quit Ollama, send a message: the UI shows the error and
   `chat_provider_errors_total{reason="unreachable"}` increments.
6. Set `OLLAMA_MODEL=nope` in `chat-service/.env` and restart the container:
   the error names the pull command and the counter records
   `reason="model_missing"`.

## Operator setup

Ollama runs on the host, so it is not provisioned by compose.

1. `winget install Ollama.Ollama`
2. Tray icon → Settings → **Expose Ollama to the network**. Ollama binds
   `127.0.0.1` by default, which `host.docker.internal` traffic cannot reach.
   The `setx OLLAMA_HOST "0.0.0.0"` route plus a tray restart is equivalent.
3. `ollama pull llama3.2:3b`
4. Verify from a container:
   `docker compose exec chat-service wget -qO- http://host.docker.internal:11434/api/tags`

**Security:** binding `0.0.0.0` exposes an unauthenticated inference API on
every interface. Anyone who can reach port 11434 can run inference against your
GPU, enumerate your models, and delete them. Acceptable behind home NAT;
on a shared or public network, add an inbound firewall rule permitting TCP
11434 only on the Docker vEthernet interface. Running `chat-service` outside
Docker against `localhost:11434` avoids the exposure entirely, at the cost of
diverging from the compose topology.

## Out of scope

System prompts, model parameters in `ChatRequest` (temperature, max tokens),
Grafana panels for the new metrics, persistence, auth, and the Claude API
itself. The `Responder` seam and the widened request argument mean each of
those lands without disturbing this design.
