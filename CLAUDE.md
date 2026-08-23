# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Aleksi Valta's engineering portfolio, headed for a public domain: three Go microservices behind an Envoy proxy, a React/TypeScript frontend speaking gRPC-Web, and a full observability stack (Prometheus, Loki, Tempo, Grafana). The streaming chat panel is becoming an assistant that answers questions about his background — the infrastructure beneath it is both the subject matter and the demonstration.

This means anything user-visible is portfolio surface: copy, error states, dashboards, and the chat's answers are read by people evaluating the work, not just by its author. Roadmap and rationale live in `docs/scalability-learning-plan.md` (untracked, local only); per-feature design docs are in `docs/superpowers/specs/` and `docs/superpowers/plans/` and are historical records of decisions — don't retro-edit them to match later framing.

## Commands

Each service needs a `.env` before compose will start it — `docker compose` uses `env_file`, not defaults:

```bash
cp order-service/.env.example order-service/.env      # same for chat-service, inventory-service
```

Stack:

```bash
docker compose up --build
docker compose up --build --scale order-service=3      # order-service is the horizontally scaled one
docker compose watch                                   # rebuilds order-service on source change
docker compose logs -f chat-service
```

Go services (each directory is its own module — `cd` in first, there is no workspace):

```bash
cd order-service && go build ./... && go test ./...
go test -run TestValidateHistory ./...                  # single test
go vet ./...
```

Frontend (`cd frontend`):

```bash
npm run dev        # Vite, opens browser
npm run build      # tsc -b && vite build
npm test           # vitest run (jsdom); npm run test:watch to iterate
npm run lint
npm run format     # prettier --write src/
```

`npm run format:check` fails on files this repo has never formatted — check
whether a warning predates your change before reformatting anything.

Endpoints when the stack is up: Envoy `:8080` (the only entry point for the frontend), Grafana `:3000` (anonymous admin), Prometheus `:9090`, RabbitMQ management `:15672`, Tempo `:3200`, Loki `:3100`, MongoDB `:27017`, inventory-service metrics `:9092`.

## Protobuf codegen

`proto/service.proto` (package `orders`) and `proto/chat.proto` (package `chat.v1`) are the source of truth. Generated code is **committed**, and each language uses a different toolchain:

- **Go** — `protoc` + `protoc-gen-go` / `protoc-gen-go-grpc`, output committed inside the consuming module (`order-service/orders/`, `chat-service/chat/`) because `go_package` is a relative `./orders` / `./chat`. The Dockerfiles copy that subdirectory explicitly, so a new service's generated package must be added to its Dockerfile.
- **TypeScript** — buf with `@bufbuild/protoc-gen-es` + `@connectrpc/protoc-gen-connect-es` (devDeps in the root `package.json`), config in `frontend/buf.gen.yaml`, output to `frontend/src/gen/`.

Changing a proto means regenerating both sides and committing the output.

## Architecture

**Request path.** Browser → Envoy `:8080` (gRPC-Web translation + CORS) → gRPC service. Envoy routes on the gRPC path prefix: `/chat.v1.ChatService/` → `chat_service_cluster`, everything else → `order_service_cluster`. Both routes set `timeout: 0s` and `grpc_timeout_header_max: 0s` — Envoy's 15s default truncates streams, so any new streaming route needs the same.

**Horizontal scaling.** `order-service` deliberately has no `container_name` and no host port mapping in `docker-compose.yml`, which is what lets `--scale` work. Envoy uses `STRICT_DNS` + `round_robin` against the `order-service` DNS name to reach all replicas; Prometheus finds them via `docker_sd_configs` with a container-name regex, rewriting the scrape target to port `9091`. Adding a port mapping or container name to that service breaks scaling.

**Async path.** `order-service.SendPacket` writes to MongoDB, then publishes JSON to the `packets` queue; `inventory-service` consumes it. The two services agree on the message shape by convention only — `order-service` marshals an inline map, `inventory-service` unmarshals into its own `DataPacket` struct. Changing one requires changing the other.

**Chat streaming.** `chat.proto` defines one server-streaming RPC emitting `ChatChunk` frames (`text_delta`… then a terminal `done`). The server is stateless: the client sends full conversation history each turn (`useChatStream.ts` caps it at `MAX_HISTORY`, `chat.go` enforces hard server-side limits since the endpoint is unauthenticated). `Responder` in `chat-service/responder.go` is the seam for the real LLM — `EchoResponder` is a zero-cost stub that replays the last user message word by word. Swapping in a Claude-backed responder should not touch the RPC handler.

## Cross-cutting conventions

- **Observability is per-service and duplicated on purpose.** There is no shared Go module, so `telemetry.go` (OTLP → Tempo), `metrics.go` (promauto collectors), and `log_trace.go` (`logWithTrace` injecting `trace_id`/`span_id` into slog) are copied into each service. Fixes to one usually belong in all of them.
- **Every service serves Prometheus metrics on `:9091`** from a goroutine started before anything else in `main`, and logs JSON via `slog.NewJSONHandler` to stdout (Promtail ships it to Loki).
- **Trace context crosses gRPC via `otelgrpc` stats handlers, and crosses RabbitMQ via `amqpHeaderCarrier`** (`amqp_carrier.go`) — inject into `amqp.Table` headers on publish, extract on consume. A new async hop must do both or the trace breaks.
- **Degrade, don't die.** Tracer init failure logs a warning and continues untraced; a RabbitMQ publish failure still returns success because the packet is already durable in MongoDB. `order-service.ensureChannel` reconnects lazily — it tries a new channel on the existing connection before a full redial, and callers must use the returned channel rather than re-reading `s.amqpChannel`.
- **Metric accounting is centralized per handler.** `chatServer.Chat` records exactly one `chatStreamsTotal` increment and one duration observation in a single deferred func, with `outcome` only ever downgraded; don't add a second Inc/Observe pair on a new exit path.
- **Client disconnects are not errors.** `classifyOutcome` treats `context.Canceled` and gRPC `codes.Canceled` alike, because a hung-up browser surfaces as either depending on where it is noticed.
- **Frontend transport is shared.** `frontend/src/api.ts` builds one `createGrpcWebTransport` pointed at Envoy and one promise client per service; auth interceptors, retries, and a env-driven baseUrl belong there, not in components.

## Comments

Comment the non-obvious *why*, in as few words as it takes. A reader who knows Go, React and gRPC does not need the *what*.

- **Three lines is the ceiling.** An inline comment gets one or two; a doc comment on an exported symbol gets up to three. Needing more means the code should be clearer, or the reasoning belongs in `docs/superpowers/specs/`.
- **Write what is true, not the story of finding it out.** Keep the constraint (`status.Errorf's %v would break the chain classifyOutcome matches on`). Drop the narrative that led to it, the alternatives rejected along the way, and the plan-task numbers.
- **Say it once.** If a doc comment already states a rule, don't restate it at the call site — cross-reference the symbol instead.
- **Delete comments that restate the code.** `// Append an empty assistant message` above a line appending an empty assistant message is noise.
- **Test comments earn their place by naming the regression**, not by re-describing the assertions: what breaks in production if this test goes red.
