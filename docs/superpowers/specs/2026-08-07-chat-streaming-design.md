# Streaming Chat Design

## Goal

Add a chat panel to the frontend backed by a new `chat-service` that streams responses token-by-token over a gRPC server-streaming RPC.

This ships with an **echo stub** responder — no LLM. It proves the transport, Envoy streaming route, incremental React render, and mid-stream cancel end to end at zero API cost. Roadmap step 3 (Claude API) swaps the responder implementation behind an interface; nothing else moves.

Prerequisite for roadmap steps 3 (LLM) and 4 (RAG) in `docs/scalability-learning-plan.md`.

## Decisions

| Question | Choice | Why |
|---|---|---|
| Transport | gRPC server-streaming over existing grpc-web stack | One request → many responses is exactly the chat shape. Reuses Envoy, buf codegen, Prometheus, Tempo. A WebSocket/Socket.IO service would add a protocol outside the proto contract, need sticky sessions once scaled, and duplicate the observability wiring. Bidi streaming is the only shape browsers can't do over grpc-web, and chat doesn't need it — interrupt is "cancel stream, start new call". |
| Service placement | New `chat-service` container | Keeps `anthropic-sdk-go`, the API key, and token metrics out of the hot packet path. LLM concurrency limits differ hard from packet throughput, so it must scale independently. |
| Conversation state | Client sends full history each turn | Server stays stateless — any replica serves any turn, no sticky routing, no Redis. Mirrors the Messages API, which is stateless the same way. |
| Persistence | None | Out of scope this round. |
| Frontend | Second panel on `MainPage` | No router dependency for a two-panel app. |

## Changes

### proto/chat.proto (new)

```proto
syntax = "proto3";

package chat.v1;
option go_package = "./chat";

service ChatService {
  // One request, many responses: server pushes ChatChunk frames until Done.
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
  // Full conversation history. Last entry is the new user turn.
  repeated Message messages = 1;
}

message ChatChunk {
  oneof event {
    string text_delta = 1;  // append to in-progress assistant message
    Done done = 2;          // terminal frame
  }
}

message Done {
  string stop_reason = 1;
  int32 input_tokens = 2;   // 0 from the echo stub; real values in step 3
  int32 output_tokens = 3;
}
```

Wire sequence for one turn:

```
ChatChunk{ text_delta: "Hello" }
ChatChunk{ text_delta: " there" }
ChatChunk{ text_delta: "!" }
ChatChunk{ done: { stop_reason: "end_turn", input_tokens: 0, output_tokens: 3 } }
```

Design notes:

- **Separate file from `service.proto`** — different service, different Go module, different container. Sharing a file would make order-service regenerate on every chat message change.
- **`chat.v1` package** (existing `orders` is unversioned) — worth versioning a streaming contract that will evolve.
- **`oneof` + `Done` from day one, not bare `stream string`.** Step 3 needs token counts, stop reasons, and cost metrics. With the terminal frame already in the contract, the stub sends zeros and step 3 fills in real numbers — the frontend never changes.
- **`Role` enum over a raw string** — typos become compile errors. Unset arrives as `ROLE_UNSPECIFIED` and is rejected by validation.

Codegen follows the existing pattern: Go bindings into `chat-service/chat/`, TypeScript into `frontend/src/gen/`.

### chat-service/ (new)

Own Go module and Dockerfile. gRPC on `GRPC_PORT=:50051`, metrics on `:9091` — matches the convention in `order-service` and `inventory-service`. Separate container, so no port collision with order-service.

| File | Responsibility |
|---|---|
| `main.go` | env load, otel init, metrics goroutine, `grpc.NewServer` with otelgrpc stats handler, listen |
| `chat.go` | `Chat(req, stream)` — validate, drive responder, `stream.Send` per delta, honor `stream.Context().Done()` |
| `responder.go` | `Responder` interface + `EchoResponder` |
| `metrics.go` | counters and histogram below |
| `telemetry.go`, `log_trace.go` | copied from `inventory-service` |

The seam that makes step 3 cheap:

```go
type Responder interface {
    Stream(ctx context.Context, history []*chatpb.Message, emit func(delta string) error) (Usage, error)
}

type Usage struct {
    StopReason   string
    InputTokens  int32
    OutputTokens int32
}
```

`EchoResponder` splits the last user message into words and emits each with a ~60ms delay. Step 3 adds `claude.go` with `ClaudeResponder` and flips one constructor in `main.go`.

`chat.go` never knows which responder it has.

**Env** (`chat-service/.env.example`):

```
GRPC_PORT=:50051
OTEL_EXPORTER_OTLP_ENDPOINT=tempo:4317
```

### docker-compose.yml

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

No published port — reached through Envoy only. Add `chat-service` to the `envoy` service's `depends_on`.

### envoy/envoy.yaml

**Add a route above the existing catch-all.** A gRPC call's URL path is `/<proto-package>.<Service>/<Method>`, so routing is prefix matching on the generated path:

```yaml
routes:
  - match: { prefix: "/chat.v1.ChatService/" }
    route:
      cluster: chat_service_cluster
      max_stream_duration:
        grpc_timeout_header_max: 0s
  - match: { prefix: "/" }        # existing — MUST stay last
    route:
      cluster: order_service_cluster
      max_stream_duration:
        grpc_timeout_header_max: 0s
```

> **Route order is load-bearing.** Envoy takes the first match and `prefix: "/"` matches everything. If the chat route is placed below it, chat requests silently reach order-service and fail as `UNIMPLEMENTED`.

**Add the cluster.** A new cluster inherits nothing from `order_service_cluster`:

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

`http2_protocol_options` because Go gRPC only speaks HTTP/2. `grpc_timeout_header_max: 0s` means no timeout ceiling — fine for a 200ms echo, required once Claude generates for 30 seconds.

The existing CORS policy and `grpc_web` filter are listener-level and already cover the new route. No change.

### prometheus/prometheus.yml

```yaml
  - job_name: chat-service
    static_configs:
      - targets: ['chat-service:9091']
```

Static, matching `inventory-service`. Switch to `docker_sd_configs` when chat-service gets scaled to multiple replicas.

### Metrics

| Metric | Type | Labels |
|---|---|---|
| `chat_streams_total` | counter | `status` = `ok` / `cancelled` / `error` |
| `chat_chunks_sent_total` | counter | — |
| `chat_stream_duration_seconds` | histogram | — |

Reserved for step 3, not implemented now: `chat_tokens_total{direction}`, `chat_api_errors_total{code}`.

Tracing comes free from the otelgrpc stats handler — one span per stream into Tempo. Add a span attribute for history message count.

### frontend/

**`src/chatClient.ts`** (new) — second transport, same `http://localhost:8080` baseUrl. Envoy demuxes by path, so no new port and no frontend routing change.

**`src/components/chat/`** (new):

- `ChatPanel.tsx` — message list, input, send/stop button
- `ChatPanel.css`
- `useChatStream.ts` — holds `messages`, `streaming`, and an `AbortController` ref

Stream consumption — connect-web returns an AsyncIterable for server-streaming methods:

```ts
for await (const chunk of client.chat({ messages }, { signal: ac.signal })) {
  // chunk.event.case === 'textDelta' | 'done'
}
```

`textDelta` appends to the in-progress assistant message via functional `setState`. `done` clears `streaming`.

Client caps history at the last 20 messages before sending.

**MainPage split** (in-scope cleanup): the current form body moves to `src/components/packet-sender/PacketSender.tsx` with its CSS; `MainPage.tsx` becomes a two-section host. Without this, `MainPage.tsx` becomes a grab bag of two unrelated features.

## Error handling

| Case | Behavior |
|---|---|
| User hits stop, or component unmounts | `AbortController` → grpc `CANCELED` → server `ctx.Done()` stops sends → `chat_streams_total{status="cancelled"}`. No error shown to user. |
| Server error mid-stream | grpc status trailer. Partial assistant text is kept, error line appended beneath it. Retry = resend the same history. |
| Empty history, or last message not `ROLE_USER` | `InvalidArgument` before any chunk is sent. |
| chat-service down | Envoy returns `UNAVAILABLE` → "chat service unavailable". |

## Testing

**Go — `chat-service/chat_test.go`**, following the existing `*_test.go` pattern in the repo:

- Table test: `EchoResponder` chunking (single word, multi-word, empty, whitespace-only)
- Stream test: fake `grpc.ServerStreamingServer[ChatChunk]` collecting sends; assert deltas concatenate to the input and a `Done` frame arrives last
- Cancel test: cancel the context mid-stream, assert sends stop and no `Done` is emitted
- Validation test: empty history and trailing-assistant-message both return `InvalidArgument`

**Frontend:** no test setup exists in the repo today; not adding one in this scope.

**Manual E2E:** `docker compose up` → send a message → confirm word-by-word render → confirm the stop button halts mid-stream → confirm `chat_streams_total` appears in Grafana and a stream span appears in Tempo.

## Out of scope

Message persistence, auth, multiple conversations, markdown rendering, retrieval/RAG, and any real LLM call. All of those are roadmap steps 3 and 4.
