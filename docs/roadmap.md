# Roadmap

ProjectCrash is a playground for scalability, infrastructure practice and AI
product integration, and the portfolio that shows them. This file tracks what
is done, what is next, and why the order is what it is. Per-feature decisions
live in `docs/superpowers/specs/`.

*Last updated: 2026-10-06*

## Status at a glance

| # | Step | Status |
|---|---|---|
| 1 | Observability | Done |
| 2 | Horizontal scaling + load testing | Done; order/inventory path now parked |
| 3 | LLM feature in the product | Done, as a portfolio assistant |
| 4 | RAG over the portfolio | In progress |
| 5 | LLM security | Not started |
| 6 | Web security | Not started |
| 7 | Terraform + going live | In progress |
| 8 | Eval set | Not started |
| 9 | Resilience patterns | Not started |
| 10 | CI/CD | Not started |
| 11 | Kubernetes | Not started |

## Done

### 1. Observability
You can't scale what you can't measure.
- Structured JSON logging with `slog`, shipped to Loki by Promtail, every line
  carrying `trace_id`/`span_id`
- Prometheus metrics on `:9091` in every service, provisioned Grafana dashboards
- OpenTelemetry tracing into Tempo, propagated over gRPC and across RabbitMQ
  via an AMQP header carrier

### 2. Horizontal scaling + load testing
- `docker compose up --scale order-service=3`, with no config edits
- Envoy `STRICT_DNS` + `round_robin` across replicas
- Prometheus `docker_sd_configs` discovers each replica; per-replica Grafana panel
- Load tested with ghz

The order-service → MongoDB → RabbitMQ → inventory-service path is **parked**:
the frontend no longer drives it, and its services are commented out of
`docker-compose.yml`. The code, Envoy route and scrape jobs stay, so it can be
restored by uncommenting.

### 3. LLM feature in the product
The original plan was to classify support tickets on the queue. It pivoted to
something a visitor can use directly: a chat assistant that answers questions
about Aleksi's background and about this stack.
- `Chat` server-streaming RPC (`chat.proto`) emitting `ChatChunk` deltas,
  routed through Envoy with stream timeouts disabled, with mid-stream cancel
- Stateless server: the client sends the history, and hard server-side limits
  apply because the endpoint is unauthenticated
- `Responder` seam with an echo stub (zero-cost load tests) and
  `OpenAIResponder` for any OpenAI-compatible provider
- Local Ollama (`llama3.2:3b`) behind the opt-in `llm` compose profile
- Metrics: input/output tokens, time to first token, classified provider errors

Dropped with the pivot: ticket classification, structured outputs, the queue
as LLM backpressure.

## In progress

### 4. RAG over the portfolio
Shipped:
- Qdrant in compose; `nomic-embed-text` embeddings over the OpenAI-compatible
  `/embeddings` API
- `ingest` job: allowlisted source walk, Markdown and Go chunkers, incremental
  re-embedding by content hash, orphan sweep
- `RetrievingResponder` condenses follow-ups into a standalone question, retrieves top-k with a
  background floor, and grounds the turn; below the score threshold nothing is
  passed to the model
- Refuses to start without a valid, non-empty index: ungrounded answers about
  a real person are worse than none
- Metrics: retrieval top score, chunk count and errors; retrieval, embed and
  condense latency
- `Warmup` RPC on page load: loads both models on Ollama, verifies the
  provider on a hosted one, then shows a fixed welcome message

Remaining:
- Hybrid dense + keyword search, once the eval set can show it helps
- Citations surfaced in the frontend
- Prompt caching on the fixed system prompt, once on a provider that supports it

## Next

### 5. LLM security
The top risk for a bot that represents a real person: a visitor makes it say
something false or embarrassing and screenshots it.
- Prompt injection: a system prompt that answers only from sources and refuses
  role-play and instruction overrides
- Forged assistant turns: the stateless server trusts client-sent history, so
  assistant messages must be validated or stripped
- Red-team the bot with adversarial questions, kept for the eval set

### 6. Web security
Classic abuse and cost controls for an unauthenticated endpoint.
- Per-IP rate limiting at Envoy
- Concurrency cap on chat streams, rejecting fast instead of queueing on the GPU
- `max_tokens` ceiling per turn, and request body limits at Envoy
- Keep crawlers from triggering `Warmup`
- Alerts on token spend from the existing token metrics

### 7. Terraform + going live
- Done: public-facing frontend restyled with Tailwind v4: full-height chat,
  header with name, role and contact links, light and dark themes
- Terraform for the host, DNS and firewall
- Hosted OpenAI-compatible provider in place of Ollama, with a spend cap
- TLS and a registered domain
- Env-driven frontend `baseUrl`

### 8. Eval set
Unit tests show the code works; an eval set shows the answers do.
- Golden questions a recruiter would ask, each with its expected source chunk
  and the facts the answer must contain
- Unanswerable questions whose correct reply is a decline, plus the red-team
  questions from LLM security
- Retrieval scored separately from answers: recall@k needs no LLM and is
  deterministic; answers are scored for faithfulness, citations and declines
- Before/after runs on the same set for any chunker, prompt or model change
- Tune `RETRIEVAL_MIN_SCORE` from the score distributions of answerable vs
  unanswerable questions, biased towards declining

## Later

### 9. Resilience patterns
- Retry with backoff on provider 429s and overload; fallback model
- Circuit breaker on gRPC calls
- Dead letter queue and idempotency keys, if the queue path comes back

### 10. CI/CD
- GitHub Actions: build → test → push images → deploy

### 11. Kubernetes
- Translate `docker-compose.yml` to manifests or Helm charts
- HPA on CPU, or on queue depth if the queue path comes back
- `kubernetes_sd_configs` + RBAC in place of `docker_sd_configs`
- Resource requests/limits, liveness/readiness probes

## Reasoning
Observability came first because it is the feedback loop for everything else.
AI came before Kubernetes because it is the skill most in demand, and because
an LLM under load reuses the observability and scaling work already done.
Going public now outranks orchestration: a portfolio nobody can reach shows
nothing. Both security steps come before it: a public, unauthenticated
endpoint about a real person needs its guardrails before its first visitor.
The eval set follows going live, so real visitor questions can seed it
alongside the hand-written ones.
