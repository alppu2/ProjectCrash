# ProjectCrash

Portfolio project by **Aleksi Valta** — a distributed microservice stack with a streaming LLM chat front and center. The chat panel is being built into an assistant that answers questions about my background and work; the infrastructure under it is the thing it answers questions about.

Deploy target: a public domain (not yet registered — see [Status](#status)).

## What's in it

| Piece | Role |
|---|---|
| `frontend` | React 19 + TypeScript + Vite. Talks gRPC-Web, renders streamed chat deltas token by token, supports mid-stream cancel. |
| `envoy` | Single public entry point. gRPC-Web ↔ gRPC translation, CORS, path-prefix routing, round-robin load balancing over service replicas. |
| `chat-service` | Go. Server-streaming `Chat` RPC emitting `ChatChunk` frames. Stateless — the client carries conversation history — so any replica can serve any turn. Replies come from an echo stub or a real model, selected by `RESPONDER`. |
| `ollama` | Optional. Runs `llama3.2:3b` locally behind an OpenAI-compatible API, so the chat answers with a real model at zero API cost. Opt-in via the `llm` compose profile. |
| `order-service` | Go. gRPC ingest: persists to MongoDB, publishes to RabbitMQ. The horizontally scaled service. |
| `inventory-service` | Go. RabbitMQ consumer, with trace context carried across the queue boundary. |
| `prometheus` / `grafana` / `loki` / `tempo` / `promtail` | Metrics, dashboards, log aggregation, distributed tracing. Every service ships all three signals. |

Service contracts live in `proto/`; generated Go and TypeScript clients are committed.

## Engineering notes

Things this stack demonstrates deliberately, rather than by accident:

- **Horizontal scaling that actually works.** `docker compose up --scale order-service=3` adds replicas with no config edits. Envoy resolves them via `STRICT_DNS` + round robin; Prometheus discovers them through `docker_sd_configs` and scrapes each one separately, so Grafana shows per-replica behaviour rather than an average.
- **Streaming as a first-class transport.** One request, many responses, over gRPC server streaming — not polling, not a bolted-on WebSocket outside the schema. Envoy's stream timeouts are explicitly disabled on those routes; the client cancels a stream by aborting it, and the server treats that as a normal outcome rather than an error.
- **Traces that survive the queue.** OpenTelemetry context propagates over gRPC via `otelgrpc`, and over RabbitMQ via a custom AMQP header carrier, so a single trace in Tempo spans browser → Envoy → order-service → MongoDB → RabbitMQ → inventory-service. Every log line carries the `trace_id` that links it back.
- **Graceful degradation.** A broker outage does not fail a write that is already durable in MongoDB; AMQP channels reconnect lazily on the next publish; a failed tracer init logs a warning and the service keeps serving.
- **Backpressure-aware LLM path.** The chat service is a separate container from the packet path on purpose: model concurrency limits and packet throughput scale on completely different curves.

## Running it locally

Requires Docker and Docker Compose.

```bash
# One-time: each service reads its own env file
cp order-service/.env.example order-service/.env
cp chat-service/.env.example chat-service/.env
cp inventory-service/.env.example inventory-service/.env

docker compose up --build

# Frontend dev server (hot reload, talks to Envoy on :8080)
cd frontend && npm install && npm run dev
```

Then: Grafana at `localhost:3000` (dashboards are provisioned, anonymous access on), Prometheus at `:9090`, RabbitMQ management at `:15672`, Tempo at `:3200`.

The chat replies with the echo stub out of the box, which needs no model and no GPU. For real model replies, set `RESPONDER=llm` in `chat-service/.env` and bring the stack up with the `llm` profile:

```bash
docker compose --profile llm up --build
```

That adds an `ollama` container which pulls `llama3.2:3b` (~2GB) on first start and serves it over the compose network — nothing is published to the host. It claims the GPU via `gpus: all`; without an NVIDIA container runtime, drop that line and it runs on CPU, slower. The same responder speaks to any OpenAI-compatible provider: point `LLM_BASE_URL` at one and set `LLM_API_KEY`, no code change.

To watch load balancing under scale:

```bash
docker compose up --build --scale order-service=3
```

## Status

Working today: the full service mesh, observability stack, horizontal scaling, end-to-end streaming chat, and real model replies from a local Ollama over an OpenAI-compatible API. The responder is a single Go interface (`chat-service/responder.go`) with two implementations — an echo stub for zero-cost load tests, and `OpenAIResponder` for any compatible provider — selected by `RESPONDER` at startup. Provider failures are classified and counted (`chat_provider_errors_total`), alongside token counts and time-to-first-token.

Next: a personal-background system prompt, retrieval over my project and work history, a hosted provider (which needs a spend cap, a `max_tokens` ceiling and rate limiting first — the `Chat` RPC is unauthenticated by design), TLS and a public domain, and Kubernetes with autoscaling driven by queue depth.

## Contact

Aleksi Valta — Valta93@hotmail.com
