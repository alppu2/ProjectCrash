# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Aleksi Valta's engineering portfolio, headed for a public domain: Go microservices behind an Envoy proxy, a React/TypeScript frontend speaking gRPC-Web, and a full observability stack (Prometheus, Loki, Tempo, Grafana). The frontend is a single streaming chat: an assistant that answers questions about his background, grounded in retrieved passages — the infrastructure beneath it is both the subject matter and the demonstration.

This means anything user-visible is portfolio surface: copy, error states, dashboards, and the chat's answers are read by people evaluating the work, not just by its author. Status, roadmap and rationale live in `docs/roadmap.md` — keep status there, not here; this file holds what changes rarely. Per-feature design docs are in `docs/superpowers/specs/` and `docs/superpowers/plans/` and are historical records of decisions — don't retro-edit them to match later framing.

**Parked:** `order-service`, `inventory-service`, MongoDB and RabbitMQ are commented out of `docker-compose.yml`. Their code, Envoy route and scrape jobs stay, and the order/async sections below describe them as they work when restored.

## Commands

Each service needs a `.env` before compose will start it — `docker compose` uses `env_file`, not defaults:

```bash
cp services/chat-service/.env.example services/chat-service/.env   # order/inventory-service too, when restored
cp infra/guard/.env.example infra/guard/.env   # HF_TOKEN; accept the Llama license on the model page first
```

Stack:

```bash
docker compose up --build                              # RESPONDER=echo: no model, no GPU
docker compose --profile llm up --build -d             # + ollama, qdrant; needs RESPONDER=llm
docker compose run --rm ingest                         # build/refresh the retrieval index; --full re-embeds all
npx promptfoo eval -c evals/redteam/promptfooconfig.yaml   # red-team suite, stack up with --profile llm
docker compose logs -f chat-service
```

With `RESPONDER=llm`, chat-service exits at startup until the model and a non-empty index are reachable, and `restart: on-failure` brings it up once they are. Without `--profile llm` that is a permanent restart loop.

When the order path is restored: `--scale order-service=3` scales it, and `docker compose watch` rebuilds it on source change.

Go services live under `services/`; infra config (Envoy, Prometheus, Grafana, Loki, Tempo, Promtail, RabbitMQ) under `infra/`. Each service is its own module — `cd` in first, there is no workspace:

```bash
cd services/chat-service && go build ./... && go test ./...
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

Endpoints when the stack is up: Envoy `:8080` (the only entry point for the frontend), Grafana `:3000` (anonymous admin), Prometheus `:9090`, Tempo `:3200`, Loki `:3100`. Ollama, Qdrant and the guard publish no host port (unauthenticated APIs); use `docker compose exec`. Restoring the order path adds RabbitMQ management `:15672`, MongoDB `:27017` and inventory-service metrics `:9092`.

## Protobuf codegen

`proto/service.proto` (package `orders`) and `proto/chat.proto` (package `chat.v1`) are the source of truth. Generated code is **committed**, and each language uses a different toolchain:

- **Go** — `protoc` + `protoc-gen-go` / `protoc-gen-go-grpc`, output committed inside the consuming module (`services/order-service/orders/`, `services/chat-service/chat/`) because `go_package` is a relative `./orders` / `./chat`. The Dockerfiles copy that subdirectory explicitly, so a new service's generated package must be added to its Dockerfile.
- **TypeScript** — buf with `@bufbuild/protoc-gen-es` + `@connectrpc/protoc-gen-connect-es` (devDeps in the root `package.json`), config in `frontend/buf.gen.yaml`, output to `frontend/src/gen/`.

Changing a proto means regenerating both sides and committing the output.

## Architecture

**Request path.** Browser → Envoy `:8080` (gRPC-Web translation + CORS) → gRPC service. Envoy routes on the gRPC path prefix: `/chat.v1.ChatService/` → `chat_service_cluster`, everything else → `order_service_cluster`. Both routes set `timeout: 0s` and `grpc_timeout_header_max: 0s` — Envoy's 15s default truncates streams, so any new streaming route needs the same.

**Horizontal scaling.** `order-service` deliberately has no `container_name` and no host port mapping in `docker-compose.yml`, which is what lets `--scale` work. Envoy uses `STRICT_DNS` + `round_robin` against the `order-service` DNS name to reach all replicas; Prometheus finds them via `docker_sd_configs` with a container-name regex, rewriting the scrape target to port `9091`. Adding a port mapping or container name to that service breaks scaling.

**Async path.** `order-service.SendPacket` writes to MongoDB, then publishes JSON to the `packets` queue; `inventory-service` consumes it. The two services agree on the message shape by convention only — `order-service` marshals an inline map, `inventory-service` unmarshals into its own `DataPacket` struct. Changing one requires changing the other.

**Chat streaming.** `chat.proto`'s `Chat` is a server-streaming RPC emitting `ChatChunk` frames (`text_delta`… then a terminal `done`). The server is stateless: the client sends full conversation history each turn (`useChatStream.ts` caps it at `MAX_HISTORY`, `internal/server/chat.go` enforces hard server-side limits since the endpoint is unauthenticated). The unary `Warmup`, called on page load by `useWarmup.ts`, readies the provider before the first turn; `RetrievingResponder` shares one in-flight warmup and reuses a success for 20 minutes (`internal/responder/warmup.go`), and echo has nothing to warm. `Responder` in `services/chat-service/internal/responder/responder.go` is the provider seam, chosen by `RESPONDER` in `responder.New`: `echo` (`EchoResponder`, zero-cost, replays the last user message) or `llm` (`RetrievingResponder` wrapping `OpenAIResponder`, for any OpenAI-compatible provider — Ollama locally, a hosted endpoint by changing `LLM_BASE_URL`/`LLM_API_KEY`). A new provider should not touch the RPC handler.

**Retrieval.** `RetrievingResponder` condenses follow-ups into a standalone question, embeds it, searches Qdrant with a reserved background floor, and grounds the turn; below `RETRIEVAL_MIN_SCORE` nothing reaches the model. The index is built by `services/chat-service/cmd/ingest` (the `ingest` compose service) over the allowlisted parts of the repo (source, `proto/`, `corpus/`, `README.md`, `docs/roadmap.md`), incremental by content hash; a file leaving the allowlist is swept from the index on the next run. The collection name is derived from the embedding model and its dimension, so changing `EMBED_MODEL` means re-ingesting.

**LLM security.** The handler signs every reply (`internal/history`; key from `HISTORY_KEY`, which replicas must share, else per-process) and drops assistant turns whose signature does not match the user turn before them, so client-sent history cannot put words in the model's mouth; dropping, not rejecting, because a stopped reply is never signed. `RetrievingResponder` runs the Prompt Guard 2 classifier (`guard` container, `internal/guard`) in parallel with retrieval over the user turns no signed reply has answered (the newest, plus orphans); a flagged turn gets the fixed `guardRefusal` and never reaches the model, and a refused exchange is dropped from later turns so one flagged message does not lock the visitor out. The guard fails closed: a turn it cannot check in time gets the fixed `guardUnavailable` and is dropped like a refusal, since under load a fail-open guard lets a flood of requests carry an attack past it. It is also required at startup. `evals/redteam/` holds the promptfoo suite and must stay outside the ingest allowlist.

**`internal/rag/sources.go` is the security boundary.** Its allowlist decides what text unauthenticated visitors can get quoted back. Add paths deliberately, never widen to a denylist, and keep `TestWalkNeverSelectsSecrets` passing; personal material goes in gitignored `corpus/`.

## Cross-cutting conventions

- **Observability is per-service and duplicated on purpose.** There is no shared Go module, so `telemetry.go` (OTLP → Tempo), `metrics.go` (promauto collectors), and `log_trace.go` (`logWithTrace` injecting `trace_id`/`span_id` into slog) are copied into each service. Fixes to one usually belong in all of them. chat-service is the exception: it has `cmd/` + `internal/` layout, so its copies are `internal/obs` (`LogWithTrace`, `InitTracer`) and `internal/server/metrics.go`.
- **Every service serves Prometheus metrics on `:9091`** from a goroutine started before anything else in `main`, and logs JSON via `slog.NewJSONHandler` to stdout (Promtail ships it to Loki).
- **Both `docker_sd_configs` regexes must survive a compose-generated name.** `prometheus.yml` and `promtail-config.yml` discover containers the same way, and relabel regexes are *fully anchored* — a bare `/(order-service)` never matches `/<project>-order-service-1`, which is what `order-service` is called because it has no `container_name`. Match with `.*` on both sides, and add the service to both files. Getting this wrong drops that service's logs or metrics silently: nothing errors, the target just never appears.
- **Trace context crosses gRPC via `otelgrpc` stats handlers, and crosses RabbitMQ via `amqpHeaderCarrier`** (`amqp_carrier.go`) — inject into `amqp.Table` headers on publish, extract on consume. A new async hop must do both or the trace breaks.
- **Degrade, don't die — except on grounding.** Optional dependencies degrade; grounding is not optional. chat-service in llm mode refuses to start without its index, and a per-turn retrieval failure swaps in `unavailableEnvelope`, which has the model say it cannot look anything up rather than answer from memory about a real person; an unknown config value is a startup error, never a silent fallback to echo. Tracer init failure logs a warning and continues untraced; a RabbitMQ publish failure still returns success because the packet is already durable in MongoDB. `order-service.ensureChannel` reconnects lazily — it tries a new channel on the existing connection before a full redial, and callers must use the returned channel rather than re-reading `s.amqpChannel`.
- **Metric accounting is centralized per handler.** `chatServer.Chat` records exactly one `chatStreamsTotal` increment and one duration observation in a single deferred func, with `outcome` only ever downgraded; don't add a second Inc/Observe pair on a new exit path.
- **A collector lives with the code that moves it.** chat-service's stream-level counters are in `internal/server/metrics.go`; everything the responder stack observes (provider, retrieval, embed, condense) is in `internal/responder/metrics.go`. Both register on promauto's default registry, so `/metrics` is unaffected by which file a collector sits in.
- **Client disconnects are not errors.** `responder.ClassifyOutcome` treats `context.Canceled` and gRPC `codes.Canceled` alike, because a hung-up browser surfaces as either depending on where it is noticed.
- **Frontend styling is Tailwind v4 over semantic tokens.** `src/index.css` defines `--bg`, `--fg`, `--accent` and friends as plain CSS variables, swapped for dark mode and exposed to Tailwind via `@theme inline` (`bg-surface`, `text-muted`…). Use those, not raw palette classes, and read the same variables when a chart library takes colours as props.
- **Frontend transport is shared.** `frontend/src/api.ts` builds one `createGrpcWebTransport` pointed at Envoy and one promise client per service (today only `chatClient`); auth interceptors, retries, and a env-driven baseUrl belong there, not in components.

## Comments

Comment the non-obvious *why*, in as few words as it takes. A reader who knows Go, React and gRPC does not need the *what*.

- **Three lines is the ceiling.** An inline comment gets one or two; a doc comment on an exported symbol gets up to three. Needing more means the code should be clearer, or the reasoning belongs in `docs/superpowers/specs/`.
- **Write what is true, not the story of finding it out.** Keep the constraint (`status.Errorf's %v would break the chain classifyOutcome matches on`). Drop the narrative that led to it, the alternatives rejected along the way, and the plan-task numbers.
- **Say it once.** If a doc comment already states a rule, don't restate it at the call site — cross-reference the symbol instead.
- **Delete comments that restate the code.** `// Append an empty assistant message` above a line appending an empty assistant message is noise.
- **Test comments earn their place by naming the regression**, not by re-describing the assertions: what breaks in production if this test goes red.
