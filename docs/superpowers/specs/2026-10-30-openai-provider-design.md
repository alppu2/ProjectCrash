# OpenAI as the hosted LLM provider — design

## Why

The site is going public on a server with no GPU, so self-hosted Ollama cannot
run there. Generation and embeddings move to OpenAI. This project makes that
switch and proves it on the local stack; going live (host, Terraform,
Kubernetes, secrets in production) is the next project and is out of scope
here.

The code is already provider-agnostic: `OpenAIResponder` speaks any
OpenAI-compatible endpoint via `LLM_BASE_URL`/`LLM_API_KEY`, and
`EMBEDDER=openai` exists for embeddings. The work is everything around that
seam: compose, config, wording, and the privacy notice.

## Goals

- `RESPONDER=llm` against OpenAI works end to end on the local stack:
  `gpt-4o-mini` for replies and condense, `text-embedding-3-small` for
  embeddings, Qdrant and the guard running locally in Docker.
- The OpenAI path needs no Ollama container, no GPU, no model pull.
- Ollama stays as an opt-in, free, offline local-dev path.
- Every "to be decided later" place that depended on the provider choice names
  OpenAI.
- Ingested docs (README, roadmap) describe the system truthfully, since the
  chat quotes them to visitors.

## Non-goals

- Kubernetes manifests, Secrets, hosting, Terraform, TLS, domain.
- Choosing the hosting provider. The privacy notice keeps it "to be named".
- The production ingest. See *Ingest gate*.
- Any change to the RPC handler, budget logic or retrieval logic.

## Anti-goals

- Ollama, its image, volume or model pulls being any part of the production
  deployment, or costing anything outside local Docker.
- OpenAI being called, and billed, by a plain `docker compose up` or by any
  test. Echo stays the zero-cost default; spend happens only when a developer
  has set `RESPONDER=llm` and a key on purpose.
- The chat telling visitors it runs on Ollama.

## Decisions (the user's)

- Provider: OpenAI. Reply model: `gpt-4o-mini`.
- Separate API keys for dev and prod. Dev key lives in
  `services/chat-service/.env` (gitignored). Prod key comes from a Kubernetes
  Secret in the go-live project; the app already reads keys from env, so a
  `secretKeyRef` needs no code change. Recorded here as a requirement for that
  project, not built.
- Ollama kept as an optional local-dev path.
- Qdrant: OpenAI embeddings go to their own collection, Ollama's collection
  left as is. No new mechanism needed — `rag.CollectionName` already derives
  the name from model id and dimension, so `text-embedding-3-small` / 1536
  lands beside `nomic-embed-text` / 768 in the same local volume.
- Production fills its own empty Qdrant volume by running ingest once after
  deploy (go-live project).
- Hosting provider: not chosen; stays pending.
- Local test first, then going live.

## Design

### Compose profiles

Today `--profile llm` starts Ollama (with `gpus: all`), and `ingest` has a hard
`depends_on: ollama`. New shape *(my call)*:

| Command | Starts | For |
|---|---|---|
| `docker compose up` | core stack, echo | default, zero cost |
| `--profile llm` | + qdrant, guard-init, guard | hosted provider (OpenAI) |
| `--profile llm --profile ollama` | + ollama | offline / free local model |

- `ollama` moves to profile `ollama` only (drop `llm`). It keeps `tools`
  only if needed for `run --rm ingest` against Ollama — builder checks how
  `tools` interacts; `ingest` must work in both modes.
- `chat-service` keeps `depends_on: ollama` with `required: false`, so it waits
  only when Ollama is part of the run.
- `ingest`'s `depends_on: ollama` gets `required: false` as well; qdrant stays
  required.
- Comments that say the guard/qdrant follow "the same reasoning as ollama"
  get reworded to stand on their own.

Verify each mode with `docker compose [--profile …] config` before and after.

### Config

`services/chat-service/.env.example` shows OpenAI as the primary llm setup
*(my call)*:

- `LLM_BASE_URL=https://api.openai.com/v1`, `LLM_MODEL=gpt-4o-mini`,
  `LLM_API_KEY=` (dev key; never committed)
- `EMBEDDER=openai`, `EMBED_BASE_URL=https://api.openai.com/v1`,
  `EMBED_MODEL=text-embedding-3-small`, `EMBED_API_KEY=`
- `RESPONDER=echo` stays the tracked default.
- Ollama values kept as a commented alternative, with the "duplicated in
  docker-compose.yml" notes attached to them.
- A short note: use a dev-only OpenAI project key with its own low monthly
  budget; the prod key belongs to a separate project.

The user edits their own `.env`; the assistant has no access to it.

Code defaults (`config.go`, `openai.go`, `cmd/ingest/main.go`) stay pointed at
Ollama *(my call)*: an unset value then fails against a missing container
instead of silently calling a paid API. Only comments that are no longer true
change.

### Ollama-specific tuning

Several values and comments are justified by a cold GPU load (~33 s) or
`OLLAMA_KEEP_ALIVE`. Builder checks each, keeps the value if it is still a
sensible bound for a hosted model, and rewords the justification so it is
true for both providers:

- `warmup.go`: `warmFreshness` (tied to `OLLAMA_KEEP_ALIVE`) and `warmTimeout`.
- `CHAT_TURN_TIMEOUT_SECONDS` comment in `.env.example`; Envoy's
  `max_stream_duration` must stay above it.
- `newLLMClient`'s header wait, `EmbedBatchSize`, the `model_missing` hint in
  `openai.go` (the `ollama pull` hint is fine when the base URL is Ollama).

Warmup stays *(my call)*: against OpenAI it costs a 1-token completion and one
embedding at most once per `warmFreshness`, shared across all visitors, and it
surfaces a bad key or base URL on page load instead of on the first question.

### Wording

Ollama becomes "local dev option", OpenAI "the provider":

- `README.md` (ingested — highest priority): component table, provider seam
  bullet, run instructions (OpenAI path first, Ollama as the opt-in), status.
- `docs/roadmap.md` (ingested): mark the hosted-provider item done; the
  Ollama line under Done; the line saying the red-team re-runs on the switch.
- `CLAUDE.md`: commands block (`--profile llm` no longer starts Ollama; add the
  `ollama` profile), endpoints paragraph, chat-streaming paragraph.
- `evals/redteam/README.md`: add a results row for `gpt-4o-mini`.
- Code comments listed above. `docs/superpowers/specs|plans` are historical and
  not edited.

### Privacy notice

`frontend/public/privacy.html`, *Who else processes it*:

- LLM provider: OpenAI. Training: API data is not used for training by
  default. Transfer basis and retention: builder drafts from OpenAI's current
  published terms (DPA, EU-US Data Privacy Framework / SCCs, API retention
  window); the user confirms against their own OpenAI account settings (e.g.
  data residency, zero-retention eligibility) before merge.
- Hosting provider: unchanged, still pending. The launch-blocker comment stays
  but names only the host.
- `docs/roadmap.md:141` updated to match: provider filled, host pending.
- `frontend/dist/` is build output; regenerate, don't hand-edit.

### Budgets

No logic change. Recorded so the next project and the docs say it plainly:

- The ledger debits provider-reported tokens (condense + reply); OpenAI
  returns usage on streams because `include_usage` is already set.
- Locally every request arrives from Docker's private gateway, so the
  developer is one visitor capped by `BUDGET_TOKENS_PER_VISITOR`.
- Embedding tokens (per-turn query, ingest) are metered by
  `chat_embed_tokens_total` but not debited.
- Counters reset on restart. The hard backstop is the monthly budget on each
  OpenAI project, set in OpenAI's dashboard, above `BUDGET_TOKENS_GLOBAL` plus
  in-flight headroom as `.env.example` already describes.

### Local verification

With the dev key in `.env`, `RESPONDER=llm`, `EMBEDDER=openai`:

1. `docker compose --profile llm up --build -d` — no Ollama container starts.
2. `docker compose run --rm ingest` with the current corpus. Until it runs,
   chat-service restart-loops on the missing collection, which is expected.
3. After ingest: a few manual turns; check budget debits, Grafana token and
   provider metrics, warmup on page load.
4. Red-team: `npx promptfoo eval -c evals/redteam/promptfooconfig.yaml`;
   structural must pass 100%, behaviour recorded as the `gpt-4o-mini` baseline.
   Decision point (user's, made after results): whether a behaviour score
   below llama3.2:3b's 11/16 blocks merge and allows system-prompt changes,
   and whether the judged slice runs with a paid OpenAI grader. The plan
   stops here and brings the results to the user.
5. Ollama mode still works: `--profile llm --profile ollama` with Ollama
   values in `.env`.
6. Unit tests, `go vet`, frontend build/lint/test as usual.

### Retrieval floor *(my call)*

`RETRIEVAL_MIN_SCORE=0.5` was tuned against `nomic-embed-text`;
`text-embedding-3-small` scores on a different scale, often lower for relevant
matches. Retuning it is config, so in scope. `.env.example` carries a value
per provider (Ollama's in its commented block). After ingest, set the OpenAI
floor from `chat_retrieval_top_score` so that on-topic questions (background,
the stack, the projects) retrieve passages and clearly off-topic ones do not.
That check is part of the acceptance for step 3 above.

### Delivery *(my call)*

One branch, merged only after local verification passes, matching "test the
provider locally first". The plan puts checkpoints where the user is needed:
dev key in `.env`, privacy wording confirmed, red-team results.
The roadmap item is marked done and the red-team row written only after the
live runs.

### Ingest gate

`corpus/background.md` is incomplete, but the user accepts it as is for the
local OpenAI ingest: it only seeds the dev index. Completing it is a gate on
the production ingest in the go-live project instead. Everything else in this
design (compose, config, wording, privacy notice) lands before the local
ingest, so the dev index is built from truthful docs.

## Requirements handed to the go-live project

- Prod `LLM_API_KEY`, `EMBED_API_KEY`, `HISTORY_KEY`, `HF_TOKEN` from
  Kubernetes Secrets; prod key in a separate OpenAI project with its own cap.
- No Ollama in production manifests.
- Initial ingest on the server's empty Qdrant volume, after
  `corpus/background.md` is completed. `corpus/` is gitignored, so it must
  reach the server by a deliberate path, not git.
- Hosting provider named in the privacy notice.

## Left to the builder

- Exact compose profile wiring that satisfies the table, including `tools`.
- Which timeout values change versus only their comments.
- Final privacy-notice wording, within the facts above.
