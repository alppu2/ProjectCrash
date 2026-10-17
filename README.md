# ProjectCrash

Portfolio project by **Aleksi Valta**: a streaming LLM chat assistant that answers questions about my background and work, running on a Go microservice stack with full observability. The infrastructure under the chat is also the thing it answers questions about.

Deploy target: a public domain (not yet registered; see [Status](#status)).

## What's in it

| Piece | Role |
|---|---|
| `frontend` | React 19 + TypeScript + Vite. A single chat panel over gRPC-Web, rendering streamed deltas token by token, with mid-stream cancel. |
| `infra/envoy` | Single public entry point. gRPC-Web ↔ gRPC translation, CORS, path-prefix routing, round-robin load balancing over service replicas. |
| `services/chat-service` | Go. Server-streaming `Chat` RPC emitting `ChatChunk` frames. Keeps no conversation state (the client carries the history, and replies are signed so it cannot be forged), so replicas sharing `HISTORY_KEY` can serve any turn. Replies come from an echo stub or a retrieval-grounded model, selected by `RESPONDER`. |
| `guard` | Meta's Prompt Guard 2 classifier, served on CPU by Hugging Face's text-embeddings-inference. Screens every visitor turn for prompt injection before it reaches the model. A one-shot `guard-init` fetches the weights, so the serving container holds no token. |
| `qdrant` | Vector store for the retrieval index, built by the `ingest` job from `corpus/` and this repository's own source. |
| `ollama` | Local development only. Serves `llama3.2:3b` and `nomic-embed-text` behind an OpenAI-compatible API, so the chat answers with a real model at zero API cost. Opt-in via the `llm` compose profile. Deployment points the same responder at a hosted endpoint instead. |
| `infra/{prometheus,grafana,loki,tempo,promtail}` | Metrics, dashboards, log aggregation, distributed tracing. Every service ships all three signals. |
| `services/{order,inventory}-service` | **Parked.** Go gRPC ingest into MongoDB and RabbitMQ, plus a queue consumer. The horizontal-scaling and async-tracing work lives here. Commented out of `docker-compose.yml` since the frontend went chat-only; uncomment to restore. |

Service contracts live in `proto/`; generated Go and TypeScript clients are committed.

## Engineering notes

Things this stack demonstrates deliberately, rather than by accident:

- **Streaming as a first-class transport.** One request, many responses, over gRPC server streaming. No polling, and no WebSocket bolted on outside the schema. Envoy's stream timeouts are explicitly disabled on those routes. The client cancels a stream by aborting it, and the server treats that as a normal outcome rather than an error.
- **Grounded, or not at all.** Each turn (follow-ups first condensed into a standalone question) is embedded and matched against Qdrant. Below a score threshold nothing is passed to the model. `chat-service` refuses to start without a valid, non-empty index: serving ungrounded answers about a real person is worse than not serving.
- **The model only hears the visitor.** Every reply is signed, and history whose assistant turns fail the check is dropped, so a client cannot put words in the model's mouth. A prompt-injection classifier screens each new turn in parallel with retrieval; a flagged turn gets a fixed refusal and never reaches the model, and the guard fails closed, so overloading it cannot sneak an attack past. A promptfoo red-team suite in `evals/redteam/` keeps the baseline honest.
- **One seam per provider.** The responder is a single Go interface (`services/chat-service/internal/responder/`) with an echo stub for zero-cost load tests, `OpenAIResponder` for any compatible provider, and `RetrievingResponder` wrapping it. Swapping Ollama for a hosted model is configuration, not code.
- **LLM observability.** Token counts, time to first token, classified provider errors, retrieval scores and per-stage latency all land in Prometheus next to the transport metrics.
- **Horizontal scaling that actually works** *(parked with order-service)*. `--scale order-service=3` adds replicas with no config edits. Envoy resolves them via `STRICT_DNS` + round robin, and Prometheus discovers each one through `docker_sd_configs`, so Grafana shows per-replica behaviour rather than an average.
- **Traces that survive the queue** *(parked with order-service)*. OpenTelemetry context propagates over gRPC via `otelgrpc` and over RabbitMQ via a custom AMQP header carrier, so a single trace spans browser → Envoy → order-service → MongoDB → RabbitMQ → inventory-service.
- **Graceful degradation.** A failed tracer init logs a warning and the service keeps serving. On the parked path, a broker outage does not fail a write already durable in MongoDB, and AMQP channels reconnect lazily.

## Running it locally

Requires Docker and Docker Compose. The optional `llm` profile additionally requires an NVIDIA GPU and the NVIDIA Container Toolkit.

```bash
# One-time: chat-service reads its own env file
cp services/chat-service/.env.example services/chat-service/.env

docker compose up --build

# Frontend dev server (hot reload, talks to Envoy on :8080)
cd frontend && npm install && npm run dev
```

Then: Grafana at `localhost:3000` (dashboards are provisioned, anonymous access on), Prometheus at `:9090`, Tempo at `:3200`, Loki at `:3100`.

The chat replies with the echo stub out of the box, which needs no model and no GPU. For real model replies, set `RESPONDER=llm` in `services/chat-service/.env`, give the guard a Hugging Face token (accept the Llama license on the Prompt Guard 2 model page first), bring the stack up with the `llm` profile, and build the retrieval index once:

```bash
cp infra/guard/.env.example infra/guard/.env   # then set HF_TOKEN
docker compose --profile llm up --build -d
docker compose run --rm ingest          # first run embeds everything; re-run after edits
docker compose run --rm ingest --full   # ignore content hashes and re-embed everything
```

With `RESPONDER=llm`, `chat-service` exits at startup until both the model and the index are reachable, and restarts on failure, so it comes up on its own once they are. Starting without `--profile llm` while `RESPONDER=llm` is set therefore leaves it in a restart loop. Switch back to `RESPONDER=echo` to run without a model.

The profile adds an `ollama` container, which pulls `llama3.2:3b` (~2GB) and `nomic-embed-text` (~274MB) on first start, a `qdrant` container holding the vectors, and the `guard` classifier. None of them publishes a host port, so their unauthenticated APIs are not reachable from outside Docker. Ollama claims the GPU via `gpus: all`. Removing that line from `docker-compose.yml` falls back to CPU inference, which works for a 3B model but answers in tens of seconds rather than a second or two.

Ollama is a local development dependency, not a deployed one. Deployment points the same responder at a hosted OpenAI-compatible endpoint: keep `RESPONDER=llm` and set `LLM_BASE_URL` and `LLM_API_KEY`, with no code changes. Qdrant and the guard are needed whichever provider generates, so they stay; only Ollama goes.

### Restoring the parked order/inventory path

Uncomment `order-service`, `inventory-service`, `mongodb` and `rabbitmq` in `docker-compose.yml`, along with envoy's `depends_on: order-service` and the `mongo-data` volume. Then copy their env files and scale:

```bash
cp services/order-service/.env.example services/order-service/.env
cp services/inventory-service/.env.example services/inventory-service/.env
docker compose up --build --scale order-service=3
```

## Status

Working today: end-to-end streaming chat, real model replies from a local Ollama over an OpenAI-compatible API grounded in passages retrieved from Qdrant, model warmup on page load, LLM security (signed history, a prompt-injection guard, a red-team baseline), and the full observability stack. The order/inventory path and its horizontal scaling are built and parked.

Next: web security (per-visitor token budgets, rate limiting and spend alerts, since the `Chat` RPC is unauthenticated by design), then a hosted provider behind a spend cap, TLS and a public domain, and an eval set for the RAG work (hybrid search and citations wait on it). Full plan and reasoning: [docs/roadmap.md](docs/roadmap.md).

## Contact

Aleksi Valta — Valta93@hotmail.com
