# ProjectCrash

Portfolio project by **Aleksi Valta**: a streaming LLM chat assistant that answers questions about my background and work, running on a Go microservice stack with full observability. The infrastructure under the chat is also the thing it answers questions about.

Deploy target: a public domain (not yet registered; see [Status](#status)).

## What's in it

| Piece | Role |
|---|---|
| `frontend` | React 19 + TypeScript + Vite. A single chat panel over gRPC-Web, rendering streamed deltas token by token, with mid-stream cancel. |
| `infra/envoy` | Single public entry point. gRPC-Web ↔ gRPC translation, CORS locked to one origin, per-IP rate limiting, a 64 KB body limit, path-prefix routing, round-robin load balancing over service replicas. |
| `services/chat-service` | Go. Server-streaming `Chat` RPC emitting `ChatChunk` frames. Keeps no conversation state (the client carries the history, and replies are signed so it cannot be forged), so replicas sharing `HISTORY_KEY` can serve any turn. Replies come from an echo stub or a retrieval-grounded model, selected by `RESPONDER`. Per-visitor and site-wide daily token budgets, concurrent stream caps and a per-turn deadline. |
| `guard` | Meta's Prompt Guard 2 classifier, served on CPU by Hugging Face's text-embeddings-inference. Screens every visitor turn for prompt injection before it reaches the model. A one-shot `guard-init` fetches the weights, so the serving container holds no token. |
| `qdrant` | Vector store for the retrieval index, built by the `ingest` job from `corpus/` and this repository's own source. |
| OpenAI | The model provider: `gpt-4o-mini` writes the replies and condenses follow-ups, `text-embedding-3-small` embeds the index and each question. Reached over the OpenAI-compatible API, so the provider is configuration. |
| `ollama` | Local development only, never deployed. Serves `llama3.2:3b` and `nomic-embed-text` behind the same API, so the chat can answer with a real model offline and at zero API cost. Opt-in via the `ollama` compose profile. |
| `infra/{prometheus,grafana,loki,tempo,promtail}` | Metrics, dashboards, log aggregation, distributed tracing. Every service ships all three signals. |
| `infra/alertmanager` | Receives the cost and health alerts Prometheus evaluates; no receiver until deployment. |
| `services/{order,inventory}-service` | **Parked.** Go gRPC ingest into MongoDB and RabbitMQ, plus a queue consumer. The horizontal-scaling and async-tracing work lives here. Commented out of `docker-compose.yml` since the frontend went chat-only; uncomment to restore. |

Service contracts live in `proto/`; generated Go and TypeScript clients are committed.

## Engineering notes

Things this stack demonstrates deliberately, rather than by accident:

- **Streaming as a first-class transport.** One request, many responses, over gRPC server streaming. No polling, and no WebSocket bolted on outside the schema. Envoy's stream timeouts are explicitly disabled on those routes. The client cancels a stream by aborting it, and the server treats that as a normal outcome rather than an error.
- **Grounded, or not at all.** Each turn (follow-ups first condensed into a standalone question) is embedded and matched against Qdrant. Below a score threshold nothing is passed to the model. `chat-service` refuses to start without a valid, non-empty index: serving ungrounded answers about a real person is worse than not serving.
- **The model only hears the visitor.** Every reply is signed, and history whose assistant turns fail the check is dropped, so a client cannot put words in the model's mouth. A prompt-injection classifier screens each new turn in parallel with retrieval; a flagged turn gets a fixed refusal and never reaches the model, and the guard fails closed, so overloading it cannot sneak an attack past. A promptfoo red-team suite in `evals/redteam/` keeps the baseline honest.
- **Cost has a ceiling per visitor, not just per site.** A hosted model's spend cap is global, so one visitor reaching it would take the assistant down for everyone. Envoy rate-limits each address; chat-service caps each visitor's daily tokens, the site's, and concurrent streams, and refuses with a reason the frontend turns into plain copy. Spend pace and cap hits alert through Alertmanager.
- **One seam per provider.** The responder is a single Go interface (`services/chat-service/internal/responder/`) with an echo stub for zero-cost load tests, `OpenAIResponder` for any compatible provider, and `RetrievingResponder` wrapping it. OpenAI in deployment and Ollama in local development run through the same code; switching is configuration.
- **LLM observability.** Token counts, time to first token, classified provider errors, retrieval scores and per-stage latency all land in Prometheus next to the transport metrics.
- **Horizontal scaling that actually works** *(parked with order-service)*. `--scale order-service=3` adds replicas with no config edits. Envoy resolves them via `STRICT_DNS` + round robin, and Prometheus discovers each one through `docker_sd_configs`, so Grafana shows per-replica behaviour rather than an average.
- **Traces that survive the queue** *(parked with order-service)*. OpenTelemetry context propagates over gRPC via `otelgrpc` and over RabbitMQ via a custom AMQP header carrier, so a single trace spans browser → Envoy → order-service → MongoDB → RabbitMQ → inventory-service.
- **Graceful degradation.** A failed tracer init logs a warning and the service keeps serving. On the parked path, a broker outage does not fail a write already durable in MongoDB, and AMQP channels reconnect lazily.

## Running it locally

Requires Docker and Docker Compose. Real model replies need an OpenAI API key, or, offline, an NVIDIA GPU with the NVIDIA Container Toolkit for the optional `ollama` profile.

```bash
# One-time: chat-service reads its own env file
cp services/chat-service/.env.example services/chat-service/.env
# An existing .env needs the budget block from .env.example; chat-service refuses to start without it.

docker compose up --build

# Frontend dev server (hot reload, talks to Envoy on :8080)
cd frontend && npm install && npm run dev
```

Then: Grafana at `localhost:3000` (dashboards are provisioned, anonymous access on), Prometheus at `:9090`, Tempo at `:3200`, Loki at `:3100`, Alertmanager at `:9093`. All of them bind to `127.0.0.1` only; Envoy `:8080` is the one public port.

The chat replies with the echo stub out of the box, which needs no model, no key and no GPU. For real model replies, set `RESPONDER=llm` in `services/chat-service/.env` and put an OpenAI key in `LLM_API_KEY` and `EMBED_API_KEY` (`.env.example` already names the models). Give the guard a Hugging Face token (accept the Llama license on the Prompt Guard 2 model page first), bring the stack up with the `llm` profile, and build the retrieval index once:

```bash
cp infra/guard/.env.example infra/guard/.env   # then set HF_TOKEN
docker compose --profile llm up --build -d
docker compose run --rm ingest          # first run embeds everything; re-run after edits
docker compose run --rm ingest --full   # ignore content hashes and re-embed everything
```

With `RESPONDER=llm`, `chat-service` exits at startup until the model, the guard and the index are reachable, and restarts on failure, so it comes up on its own once they are. Starting without `--profile llm` while `RESPONDER=llm` is set therefore leaves it in a restart loop. Switch back to `RESPONDER=echo` to run without a model.

The profile adds a `qdrant` container holding the vectors and the `guard` classifier. Neither publishes a host port, so their unauthenticated APIs are not reachable from outside Docker.

On cost: chat-service debits the tokens OpenAI reports for each turn (condense and reply) against the per-visitor and site-wide daily budgets. Embedding tokens are metered (`chat_embed_tokens_total`) but not debited, and the counters reset on restart, so the OpenAI project's monthly budget is the hard ceiling. Locally every request arrives from Docker's gateway, so you are one visitor. Use a dev-only OpenAI project and key, separate from production's.

**Offline, with Ollama.** Swap in the commented Ollama block from `.env.example` and add the `ollama` profile; it pulls `llama3.2:3b` (~2GB) and `nomic-embed-text` (~274MB) on first start and claims the GPU via `gpus: all`. Removing that line falls back to CPU inference, which works for a 3B model but answers in tens of seconds. Each embedding model gets its own Qdrant collection, so switching back and forth needs no re-ingest once both are built.

```bash
docker compose --profile llm --profile ollama up --build -d
docker compose --profile ollama run --rm ingest
```

Ollama is a local development dependency only; nothing deployed runs it.

### Restoring the parked order/inventory path

Uncomment `order-service`, `inventory-service`, `mongodb` and `rabbitmq` in `docker-compose.yml`, along with envoy's `depends_on: order-service` and the `mongo-data` volume. Then copy their env files and scale:

```bash
cp services/order-service/.env.example services/order-service/.env
cp services/inventory-service/.env.example services/inventory-service/.env
docker compose up --build --scale order-service=3
```

## Status

Working today: end-to-end streaming chat, real model replies from OpenAI (`gpt-4o-mini`), or a local Ollama in development, grounded in passages retrieved from Qdrant, model warmup on page load, LLM security (signed history, a prompt-injection guard, a red-team baseline), web security (per-visitor token budgets, per-IP rate limiting, stream caps and spend alerts, since the `Chat` RPC is unauthenticated by design), and the full observability stack. The order/inventory path and its horizontal scaling are built and parked.

Next: TLS and a public domain, and an eval set for the RAG work (hybrid search and citations wait on it). Full plan and reasoning: [docs/roadmap.md](docs/roadmap.md).

## Contact

Aleksi Valta — Valta93@hotmail.com
