# Retrieval-Augmented Chat over Qdrant — Design

**Date:** 2026-09-08
**Status:** Approved, not yet implemented

Replace the whole-corpus system prompt with retrieval. `background.md` and this
repository's own source become chunks in a vector database; each turn embeds the
user's question, searches for the passages that answer it, and grounds the reply
in those alone. Roadmap step 4 in `docs/scalability-learning-plan.md`.

Prerequisites, both shipped: the `Responder` seam
(`2026-08-09-ollama-responder-design.md`) and the corpus envelope it grounds
against (PR #3, `feat/portfolio-corpus`). That spec anticipated this one —
"extract when roadmap step 4 adds a second Ollama call site (embeddings)".

The roadmap's step 4 predates the pivot away from ticket classification. It
names semantic search over tickets and Voyage AI for embeddings; neither
survives. Tickets were never built, and Ollama serves embeddings itself.

## Why retrieval at all

`corpus/background.md` is 2754 bytes against a 16384-byte cap. It fits in the
prompt today with room to spare, so retrieval buys no answer quality yet — it
buys the architecture that still works when the corpus is 150KB, and it is the
subject matter this portfolio is about.

Retrieval therefore has to be real rather than decorative. A design that keeps
stuffing the background and retrieves only the overflow would demonstrate
nothing; the pure path is the one worth building, and its weak spot — vague
questions retrieving badly — is solved inside retrieval by a quota rather than
outside it by a static prompt.

## Decisions

| Question | Choice | Why |
|---|---|---|
| Vector database | Qdrant, compose service, no published host port | Exposes `/metrics` for the existing Prometheus scrape, so the stack's observability story extends to it for free. No host port for the same reason as Ollama: an unauthenticated API stays off the host. |
| Embedding provider | Ollama, second model, `/v1/embeddings` | Already in the stack, already GPU-backed, no key and no egress. Hosted embeddings are better but the gap is invisible at this corpus size. `EMBED_*` config is independent of `LLM_*`, so a hosted embedder is a config change. |
| Embedding model | `nomic-embed-text`, 768 dims | ~274MB alongside llama3.2:3b's ~2GB on a 6GB GTX 1060. 8192-token input window covers any chunk we produce. |
| Collection layout | One collection, `payload.kind` separating background from source | Qdrant's own recommended pattern is payload filtering over collection-per-tenant. One search per turn, no merge logic. Two content collections would double again per embedder. |
| Collection naming | Derived: `corpus__<model>__<dims>`, never configured | A free-form name is one typo away from searching a space built by a different model. Same dimension and different model does not error — plausible scores, wrong answers. |
| Prompt composition | Pure retrieval. Nothing stuffed but `corpusEnvelope` | Stuffing is not what a corpus larger than the context window can do, and the point of the slice is the real shape. Accepts a hard dependency on Qdrant. |
| Vague-query floor | Retrieval quota: at least 2 of `top_k` reserved for `kind="background"` | "Who are you?" has no semantic anchor and retrieves arbitrarily. The quota keeps identity answerable without a static prompt or duplicated text. |
| Ingest | `cmd/ingest`, run on demand, exits | Serving stays decoupled from indexing. Indexing at startup would grow boot time with the corpus and re-embed on every restart; a dedicated service would add a fourth copy of the telemetry triple for a job that runs in seconds. |
| Retrieval toggle | None. Implied by `RESPONDER=llm` | One axis fewer, and no configuration in which a model-backed deploy silently answers ungrounded. |
| Qdrant unreachable at startup | Startup error | With nothing stuffed there is no grounding to fall back to. Serving ungrounded answers about a real person is worse than not serving. |

## Architecture

```
INGEST  (cmd/ingest — run, print, exit)

  sources.go ──> chunk.go ──> embed.go ──HTTP──> Ollama /v1/embeddings
   allowlist      md + Go       batch
      walk       chunkers         │
                                  └──> qdrant.go ──HTTP──> Qdrant
                                        upsert + sweep

QUERY   (chat-service — per turn, ahead of responder.Stream)

  ChatRequest ──> retrieve.go ──> embed.go ──────> Ollama /v1/embeddings
                   condense                            │
                       │                               ▼
                       │                          qdrant.go ──> Qdrant
                       │                        quota'd search
                       ▼
                  assemble prompt ──> openai.go ──> Ollama /v1/chat/completions
```

`chunk.go`, `embed.go` and `qdrant.go` are shared by both paths. Ingest never
serves; chat-service never writes.

`proto/chat.proto`, `envoy.yaml`, `chatServer.Chat` and every frontend file stay
as they are. Retrieval produces a `System` string ahead of `responder.Stream`,
so the RPC handler does not learn about Qdrant.

### Changes

| File | Change |
|---|---|
| `chat-service/embed.go` | New. `Embedder` interface + OpenAI-shaped implementation. |
| `chat-service/qdrant.go` | New. Collection assert, upsert, search, scroll, delete-by-filter. |
| `chat-service/chunk.go` | New. Markdown and Go chunkers. Pure functions. |
| `chat-service/sources.go` | New. Allowlisted walk. |
| `chat-service/retrieve.go` | New. Condensation, search, threshold, prompt assembly. |
| `chat-service/cmd/ingest/main.go` | New. Ingest entry point. |
| `chat-service/corpus.go` | Shrinks to `corpusEnvelope` and the citation rule. `loadCorpus`, `stubCorpus`, `maxCorpusBytes` and `corpus/stub.md` retire. |
| `chat-service/main.go` | Builds the `Embedder` and Qdrant client under `RESPONDER=llm`; startup assertions. |
| `chat-service/metrics.go` | Six new metrics. |
| `chat-service/Dockerfile` | `COPY chat-service/cmd/ cmd/` and a second `go build -o /ingest ./cmd/ingest`. The existing `COPY chat-service/*.go ./` is flat and would miss the subdirectory — the same trap the generated-package copy already documents. The `corpus/stub.md` copy goes with the stub. |
| `chat-service/.env.example` | `EMBEDDER`, `EMBED_*`, `QDRANT_URL`, `RETRIEVAL_*`. `CORPUS_PATH` retires. |
| `docker-compose.yml` | `qdrant` service; `ingest` service under `profiles: [tools]`; second Ollama pull. |
| `prometheus.yml` | Scrape `qdrant`. |

`corpus/background.md` stays gitignored and stays mounted — into the ingest
container now, read-only, rather than into chat-service.

Retiring the stub is safe: plain `docker compose up` still defaults to
`RESPONDER=echo`, which needs neither Ollama nor Qdrant, so the README's
one-liner onboarding is unchanged. The stub existed to keep a system prompt
non-empty, and there is no longer a corpus in the system prompt.

### Configuration

```
EMBEDDER=ollama                       # ollama | openai, unknown is a startup error
EMBED_BASE_URL=http://ollama:11434/v1
EMBED_MODEL=nomic-embed-text
EMBED_API_KEY=                        # empty for a local Ollama

QDRANT_URL=http://qdrant:6333
RETRIEVAL_TOP_K=6
RETRIEVAL_MIN_SCORE=0.5               # tune from the score histogram
RETRIEVAL_BACKGROUND_FLOOR=2
```

`EMBED_*` mirrors `LLM_*` deliberately, including `validateBaseURL`'s rejection
of embedded credentials. The two are independent axes: local embeddings with
hosted generation is a plausible production shape.

The collection name is absent on purpose — see the decisions table.

## Ingest

### Sources

```go
roots = corpus, proto, docs/superpowers, order-service, chat-service,
        inventory-service, frontend/src, CLAUDE.md, README.md
exts  = .md, .go, .ts, .tsx, .proto
skip  = node_modules, dist, gen, orders, chat        // build output, generated pb
```

A file is indexed only if it sits under an allowed root, carries an allowed
extension, and is under no skipped directory. The allowlist is what keeps `.env`
out — it has no allowed extension, so no deny rule has to remember it. A
denylist would fail open on the file nobody thought of, and this database is
quoted back to unauthenticated visitors.

`_test.go` is excluded: assertions retrieve poorly and crowd real code out of
`top_k`.

The repository mounts read-only at `/workspace` in the ingest container.

### Chunking

Markdown splits on headings, then packs paragraphs to roughly 300 tokens with
about 50 tokens of overlap, so a sentence spanning a boundary stays retrievable
from either side. The heading text is prepended to each chunk, which gives an
isolated chunk enough context to embed meaningfully.

Go parses with `go/parser` and chunks per top-level declaration — `func`, `type`,
`const`/`var` block — carrying the package clause and the declaration's doc
comment. Oversized functions split at statement boundaries.

`background.md` has no headings today. They are the primary chunk boundary and
should be added as it is written.

### Points

```
id      = uuidv5(namespace, source_path + ":" + chunk_index)
payload = kind          "background" | "source" | "docs"
          text          chunk content
          source        "order-service/amqp_carrier.go"
          line          17
          symbol        "amqpHeaderCarrier.Set"     (Go only)
          heading       "Async path"                (markdown only)
          content_hash  sha256(file bytes + chunkerVersion)
          run_id        uuid4 of this ingest run
```

Qdrant accepts only an unsigned integer or a UUID as a point ID, so the readable
form is hashed into a UUIDv5 rather than used directly. Deterministic IDs are
what make re-running ingest an overwrite instead of an append.

### Incremental ingest

Per-file `content_hash` decides whether a file is re-chunked and re-embedded.
`chunkerVersion` is part of the hash: changing `chunk.go` leaves file bytes
identical, and a hash over bytes alone would skip every file and leave the index
built by the previous chunker.

Deterministic IDs prevent duplicates but not orphans. A file edited from eight
chunks down to five leaves three stale points that are still searchable and now
lie; a file deleted outright is never visited again and lingers entirely. The
sweep handles both, in this order:

1. The walk completes successfully. A run that failed partway must sweep
   nothing — its unvisited sources are indistinguishable from deleted ones.
2. Points belonging to hash-skipped files are restamped with the current
   `run_id`. This is a payload update, not a re-embed. Skipping it makes the
   sweep delete exactly the files that were unchanged.
3. `delete(filter: run_id != currentRun)`.

```
$ docker compose run --rm ingest
  scanned   47 files
  skipped   41 unchanged
  embedded  38 chunks from 6 files
  swept      6 orphaned points
  total    412 points in corpus__nomic-embed-text__768

$ docker compose run --rm ingest --full     # ignore hashes, re-embed everything
```

Ingest is not triggered automatically. A git hook, `docker compose watch`, or a
CI step are all plausible later; during active development any of them would
re-embed on every save.

## Query path

### Condensation

A follow-up turn carries its meaning in the history, not in its own words.
Embedding "what about testing?" on its own retrieves nothing useful. Before
searching, the history and the new turn are folded into one standalone question
by a small completion against the same model.

Skipped on the first turn, where there is no history to fold in. On failure the
raw last user message is used instead: degraded retrieval beats no answer. Its
latency is measured separately, because it lands ahead of the first token and
would otherwise hide inside `chat_time_to_first_token_seconds`.

### Search

```
search A: filter kind="background", limit RETRIEVAL_BACKGROUND_FLOOR
search B: unfiltered, limit RETRIEVAL_TOP_K
merge, dedupe by id, truncate to RETRIEVAL_TOP_K
drop everything scoring below RETRIEVAL_MIN_SCORE
```

Two searches rather than one because the floor has to be guaranteed, not hoped
for. The quota is what makes a stuffed background unnecessary.

The threshold is the honesty floor. A question the corpus cannot answer must
retrieve nothing, so the envelope's existing "say so plainly" rule takes over,
rather than handing the model the five least-bad chunks and inviting invention.

`RETRIEVAL_MIN_SCORE` starts at 0.5 and is a guess. The score histogram is
exported so it can be tuned from Grafana against real questions instead of
argued about in a constant.

### Prompt

```
corpusEnvelope
Sources:
  [1] (corpus/background.md § Work history) ...
  [2] (order-service/amqp_carrier.go:17 amqpHeaderCarrier.Set) ...
<history>
<user turn>
```

`corpusEnvelope` gains one rule — cite the bracketed number for each claim — and
loses nothing. Its anti-invention rules already say the right thing, including
the one about earlier assistant turns not being evidence.

With zero chunks over threshold the Sources block is absent rather than empty.

## Error handling

| Failure | Behaviour |
|---|---|
| Qdrant unreachable at startup | Startup error. |
| Collection missing | Startup error naming the ingest command. |
| Collection dimension ≠ `Embedder.Dims()` | Startup error. |
| Collection empty | Startup error. Otherwise it presents as a chat that works but knows nothing. |
| Qdrant unreachable mid-stream | Degrade, don't die: stream a reply whose system prompt states the knowledge base is unavailable and that it cannot answer now. |
| Embedder unreachable mid-stream | As above — no query vector, no search. |
| Zero chunks over threshold | Not an error. No Sources block; the envelope handles it. |
| Condensation fails | Fall back to the raw last user message. |
| Client disconnect | Unchanged. `classifyOutcome` still needs the bare context error, so retrieval must not wrap cancellation in a status. |

The startup assertions are deliberately strict and the mid-stream paths
deliberately soft: a misconfigured deploy should never start, and a healthy one
should not lose a conversation to a transient blip.

## Metrics

```
chat_retrieval_duration_seconds      histogram   search wall time
chat_retrieval_chunks                histogram   chunks surviving the threshold
chat_retrieval_top_score             histogram   tunes RETRIEVAL_MIN_SCORE
chat_retrieval_errors_total{reason}  counter     unreachable | decode_error | dim_mismatch
chat_embed_duration_seconds          histogram
chat_condense_duration_seconds       histogram   its share of time to first token
```

Qdrant's own `/metrics` is scraped alongside. Both `prometheus.yml` and
`promtail-config.yml` use fully anchored regexes, so the new service needs `.*`
on both sides of its name in whichever file it is added to.

`chatServer.Chat` keeps recording exactly one `chatStreamsTotal` increment and
one duration observation in its single deferred func. Retrieval metrics are
their own series, never a second pair on a new exit path.

Ingest is a CLI. It prints its summary and exports nothing.

## Testing

| Unit | Approach |
|---|---|
| `chunk.go` | Table tests and golden files. Pure and I/O-free, so the highest-value tests here. |
| `sources.go` | Walk a fixture tree containing a `.env`. It must never be selected. |
| `embed.go` | `httptest`, same shape as `openai_test.go`. |
| `qdrant.go` | `httptest`: upsert, scroll, delete-by-filter, dimension mismatch. |
| `retrieve.go` | Fake `Embedder` and fake searcher. Quota holds, threshold drops, prompt numbering, condensation fallback. |
| Ingest idempotency | Fake Qdrant. Two runs give one point count; a shrunk file sweeps its orphans; a failed walk sweeps nothing. |

No live Qdrant or Ollama in unit tests — `go test ./...` must pass with neither
running, as it does today. An integration test behind a build tag can exercise
the real pair.

### Manual verification

1. `docker compose --profile llm up --build`, then
   `docker compose run --rm ingest`.
2. Ask a background question, a source-code question, and something the corpus
   cannot answer. The third must decline rather than invent.
3. Ask a follow-up that depends on the previous turn ("what about testing?") and
   confirm condensation retrieved sensibly.
4. Edit a source file, re-run ingest, confirm only that file is re-embedded.
5. Delete a source file, re-run ingest, confirm its points are swept.
6. Stop Qdrant mid-conversation and confirm the reply degrades instead of
   hanging.

## Out of scope

Each is a clean follow-up with something to measure:

- Hybrid search — dense and BM25 sparse vectors, both native to Qdrant in one
  query. The obvious next increment: keyword-exact matches are precisely what
  dense retrieval is worst at, and source code is full of them.
- Cross-encoder reranking over the retrieved set.
- MMR or another diversity pass, so several near-identical chunks cannot fill
  `top_k`.
- A retrieval eval harness — golden question-to-chunk pairs, recall@k in
  Grafana. Everything above should be judged against it rather than by feel.
- A second collection populated by a hosted embedder, A/B'd against the local
  one on the same queries.
- Citations surfaced in the frontend, which needs a new `ChatChunk` event and so
  a proto change on both sides.
- Automatic ingest triggering.
