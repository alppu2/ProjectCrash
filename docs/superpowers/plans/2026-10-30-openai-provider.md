# OpenAI Provider Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run `RESPONDER=llm` against OpenAI (`gpt-4o-mini`, `text-embedding-3-small`) on the local stack with no Ollama, keep Ollama as an opt-in dev profile, and make every provider-dependent text name OpenAI.

**Architecture:** No new code paths: `OpenAIResponder` and `OpenAIEmbedder` already speak OpenAI. The work is compose profile wiring, `.env.example`, comment and doc wording, the privacy notice, then a live local verification with user checkpoints.

**Tech Stack:** Docker Compose v5, Go (chat-service), static HTML (privacy notice), promptfoo.

**Spec:** `docs/superpowers/specs/2026-10-30-openai-provider-design.md`

## Global Constraints

- Reply model `gpt-4o-mini`; embedding model `text-embedding-3-small`; base URL `https://api.openai.com/v1` for both.
- `RESPONDER=echo` stays the tracked default; plain `docker compose up` must start no ollama, qdrant or guard and call no paid API.
- No `.env` file is read or written by the implementer (hook-enforced). Only `.env.example`. The user edits `services/chat-service/.env`.
- Code defaults in `config.go`, `openai.go`, `cmd/ingest/main.go` keep pointing at Ollama. Only comments change in Go.
- No changes to the RPC handler, budget logic or retrieval logic.
- `docs/superpowers/specs/` and `docs/superpowers/plans/` other than this plan's own spec are not edited.
- Comment rules from `CLAUDE.md`: why not what, ≤3 lines, no narrative.
- Privacy notice keeps the hosting provider "to be named before launch".
- Commits end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

- `docker compose run --rm ingest` with only `llm`/`tools` active must not start or wait on ollama (verified: an out-of-profile dependency with `required: false` is dropped). Task 1 checks it.
- `--profile llm --profile ollama` must still bring chat-service up against Ollama after the split. Task 5 step checks it.
- Ingested README/roadmap must not claim Ollama serves the public chat; Task 3 greps for it.
- A relevant question on OpenAI embeddings scoring under the old 0.5 floor silently degrades grounding; Task 5 retunes it.
- The privacy notice states facts about OpenAI's terms; the user confirms them before merge (Task 4 checkpoint).

---

### Task 1: Split compose profiles

**Files:**
- Modify: `docker-compose.yml` (chat-service `depends_on` comments ~51-69, `ollama` profiles ~75-79, qdrant/guard comments ~130-133/~176-178/~186, ingest `depends_on` ~221-229)
- Modify: `CLAUDE.md` (Commands block line 26, endpoints paragraph line 59)

**Interfaces:**
- Produces: profiles `llm` = qdrant + guard-init + guard; `ollama` = ollama; `tools` = qdrant + ingest. Later tasks' docs use these names.

- [ ] **Step 1: Record current service sets**

Run from repo root:
```bash
for p in "" "--profile llm" "--profile tools" "--profile llm --profile ollama"; do echo "== $p"; docker compose $p config --services | sort; done
```
Expected now: `--profile llm` includes `ollama`; `--profile llm --profile ollama` errors or equals llm (profile unknown).

- [ ] **Step 2: Edit compose**

- `ollama.profiles`: `[ollama]` only. Rewrite its opt-in comment: `docker compose --profile llm --profile ollama up`; free offline model, never deployed.
- `ingest.depends_on.ollama`: add `required: false` with a one-line why (waits only in Ollama mode). Rewrite the "Both also carry the tools profile" comment to cover qdrant only.
- chat-service `depends_on` comments: ollama is now optional because it has its own profile; qdrant and guard comments stand alone (echo default must not wait for them), not "same reasoning as ollama".
- qdrant comment "Same profile as ollama" → retrieval is implied by `RESPONDER=llm`. Guard comment: drop "like ollama's".

- [ ] **Step 3: Verify service sets**

Re-run Step 1's loop. Expected:
- default: no `ollama`, `qdrant`, `guard`, `guard-init`, `ingest`
- `--profile llm`: `qdrant`, `guard-init`, `guard`; no `ollama`
- `--profile tools`: `qdrant`, `ingest`; no `ollama`
- `--profile llm --profile ollama`: llm set plus `ollama`

- [ ] **Step 4: Update CLAUDE.md commands and endpoints**

Line 26 becomes the hosted line `# + qdrant, guard; needs RESPONDER=llm and an OpenAI key` and add a line `docker compose --profile llm --profile ollama up --build -d   # + ollama: free local model, GPU`. Endpoints paragraph: keep the no-host-port statement for all three.

- [ ] **Step 5: Commit**

```bash
git add docker-compose.yml CLAUDE.md
git commit -m "feat(compose): move ollama to its own opt-in profile"
```

### Task 2: OpenAI-first `.env.example` and provider-neutral comments

**Files:**
- Modify: `services/chat-service/.env.example`
- Modify: `services/chat-service/internal/responder/warmup.go:16-20`
- Modify: `services/chat-service/internal/responder/openai.go:20-32` (comments only)
- Modify: `services/chat-service/internal/rag/embed.go:17-18,31,35` (comments only)
- Modify: `services/chat-service/cmd/chat-service/config.go:46-49` comment if any states Ollama is the provider

- [ ] **Step 1: Rewrite the LLM/embedding block of `.env.example`**

Active values: `LLM_BASE_URL=https://api.openai.com/v1`, `LLM_MODEL=gpt-4o-mini`, `LLM_API_KEY=`, `EMBEDDER=openai`, `EMBED_BASE_URL=https://api.openai.com/v1`, `EMBED_MODEL=text-embedding-3-small`, `EMBED_API_KEY=`. `RESPONDER=echo` unchanged.
Commented alternative block for `--profile ollama`: `LLM_BASE_URL=http://ollama:11434/v1`, `LLM_MODEL=llama3.2:3b`, `EMBEDDER=ollama`, `EMBED_BASE_URL=http://ollama:11434/v1`, `EMBED_MODEL=nomic-embed-text`, keys empty; the "duplicated in docker-compose.yml" notes move onto these lines.
Key comment: dev key from a dev-only OpenAI project with its own monthly budget; prod uses a separate project and key from a Kubernetes Secret; never commit a key.
`RETRIEVAL_MIN_SCORE`: active value stays 0.5 until Task 5 tunes it; add commented `# Ollama (nomic-embed-text): RETRIEVAL_MIN_SCORE=0.5` and a note that each embedding model scores on its own scale.
`CHAT_TURN_TIMEOUT_SECONDS`: keep 120; reword "Above a cold model load (~33s)" so it covers both (Ollama cold load, hosted provider latency tail).

- [ ] **Step 2: Reword Go comments**

- `warmup.go`: `warmFreshness` stays 20m — justify as "below Ollama's OLLAMA_KEEP_ALIVE (30m); for a hosted provider it bounds how stale a credential check may be". `warmTimeout` stays 120s; comment covers the Ollama cold load as the worst case.
- `openai.go`: defaults comment says the defaults are the local Ollama profile, chosen so an unset value fails against a missing container rather than calling a paid API. `Model` example: `gpt-4o-mini`. `APIKey`: "empty for Ollama".
- `embed.go`: `OpenAIEmbedder` doc: "OpenAI in deployment, Ollama in the local profile". `EmbedBatchSize` comment already provider-neutral enough; leave unless it misstates.
- `openai.go`: `newLLMClient`'s header-wait comment and the `model_missing` hint (`ollama pull` advice) stay valid for Ollama; reword only if they read as if Ollama is the only provider.

- [ ] **Step 3: Verify build and tests**

```bash
cd services/chat-service && go build ./... && go vet ./... && go test ./...
```
Expected: all pass (comment-only Go changes).

- [ ] **Step 4: Commit**

```bash
git add services/chat-service
git commit -m "feat(chat): OpenAI as the documented llm provider, Ollama the local option"
```

### Task 3: Provider wording in ingested and contributor docs

**Files:**
- Modify: `README.md` (lines ~16, 31, 54-67, 81)
- Modify: `docs/roadmap.md` (lines ~57, 77, 112, 126, 185)
- Modify: `CLAUDE.md` (Chat streaming paragraph, line 78)
- Modify: `evals/redteam/README.md` only if prose (not the results table) names Ollama as the provider

- [ ] **Step 1: README**

Component table: add/adjust so OpenAI generates and embeds; the `ollama` row says local development only, opt-in via the `ollama` profile. Run instructions: OpenAI path first (`RESPONDER=llm`, dev key in `LLM_API_KEY`/`EMBED_API_KEY`, `--profile llm`), then the Ollama alternative (`--profile llm --profile ollama`, swap to the commented `.env.example` values, GPU note). Status line: "real model replies from OpenAI (`gpt-4o-mini`), or a local Ollama in development". One short cost paragraph: the ledger debits OpenAI-reported condense + reply tokens; embedding tokens are metered (`chat_embed_tokens_total`) but not debited; counters reset on restart, so the OpenAI project's monthly budget is the hard backstop.

- [ ] **Step 2: Roadmap**

Line 126 item: leave it open (Task 6 marks it done). Line 57 Done entry stays historical. Line 112 Warmup entry: provider-neutral. Line 185 stays (Ollama is still a container). Line 141: "provider filled (OpenAI); host still pending". Under the going-public section add the go-live requirements from the spec: prod `LLM_API_KEY`/`EMBED_API_KEY`/`HISTORY_KEY`/`HF_TOKEN` from Kubernetes Secrets, prod key in its own OpenAI project with its own cap, no Ollama in production, initial ingest on the empty Qdrant volume after `corpus/background.md` is completed and delivered outside git.

- [ ] **Step 3: CLAUDE.md chat-streaming paragraph**

"…for any OpenAI-compatible provider — OpenAI in deployment, Ollama via the `ollama` profile locally…"

- [ ] **Step 4: Verify nothing ingested says Ollama serves the chat**

```bash
grep -niE "ollama|llama3" README.md docs/roadmap.md
```
Expected: every hit describes Ollama as the local/dev option or is a historical Done entry.

- [ ] **Step 5: Commit**

```bash
git add README.md docs/roadmap.md CLAUDE.md evals/redteam/README.md
git commit -m "docs: OpenAI as the provider, Ollama as the local option"
```

### Task 4: Privacy notice names OpenAI

**Files:**
- Modify: `frontend/public/privacy.html:138-152`

- [ ] **Step 1: Look up OpenAI's current terms**

From openai.com (API data usage policy, DPA, enterprise privacy page): whether API inputs are used for training by default; default abuse-monitoring retention window for API data; transfer mechanism for EU→US (EU-US Data Privacy Framework certification and/or SCCs in the DPA). Note the URL for each fact.

- [ ] **Step 2: Edit the LLM provider item**

Replace the three `pending` spans: provider `OpenAI (OpenAI Ireland Ltd for EEA users, if that is what the terms say; otherwise OpenAI, L.L.C.)`, transfer basis, training statement, retention window, with a link to OpenAI's privacy/API data page. Hosting item untouched. Launch-blocker comment reworded to name only the host.

- [ ] **Step 3: Verify**

```bash
grep -n "pending" frontend/public/privacy.html
cd frontend && npm run build
```
Expected: only the `.pending` CSS rule and the hosting span remain; build passes.

- [ ] **Step 4: Checkpoint — user confirms wording**

Show the user the rendered paragraph plus the source URLs. Ask them to confirm against their OpenAI account (data residency, zero-retention eligibility). Apply corrections.

- [ ] **Step 5: Commit**

```bash
git add frontend/public/privacy.html
git commit -m "feat(frontend): name OpenAI in the privacy notice"
```

### Task 5: Local verification against OpenAI

**Files:**
- Modify: `services/chat-service/.env.example` (`RETRIEVAL_MIN_SCORE` active value)

- [ ] **Step 1: Checkpoint — user sets `.env`**

Ask the user to copy the Task 2 OpenAI block into `services/chat-service/.env` with their dev key in `LLM_API_KEY` and `EMBED_API_KEY`, set `RESPONDER=llm`, and set a monthly budget on the dev OpenAI project. Wait for confirmation.

- [ ] **Step 2: Bring up the hosted stack**

```bash
docker compose --profile llm up --build -d
docker compose ps --format '{{.Service}}'
```
Expected: no `ollama`; `qdrant`, `guard` healthy; chat-service restarting (no collection yet).

- [ ] **Step 3: Ingest with the current corpus**

```bash
docker compose run --rm ingest
```
Expected: no ollama container started; logs a collection name containing `text_embedding_3_small` and `1536`; chat-service becomes Up afterwards.

- [ ] **Step 4: Manual turns and metrics**

Through the frontend (`cd frontend && npm run dev`): ask on-topic questions (background, the stack, a project) and two off-topic ones. In Prometheus/Grafana check `chat_retrieval_top_score`, provider token counters, `chat_embed_tokens_total`, and that chat-service logs a budget debit per turn. Warmup succeeds on page load.

- [ ] **Step 5: Tune `RETRIEVAL_MIN_SCORE`**

Pick the floor between the lowest on-topic top score and the highest off-topic top score from Step 4. Set it in `.env.example` (active, OpenAI) and ask the user to set it in `.env`; restart chat-service and re-ask one question of each kind. Expected: on-topic retrieves sources, off-topic gets the "say so plainly" answer.

- [ ] **Step 6: Ollama mode regression**

User swaps `.env` to the commented Ollama block; run `docker compose --profile llm --profile ollama up -d`; one turn answers; user swaps back. Expected: works as before the split.

- [ ] **Step 7: Commit**

```bash
git add services/chat-service/.env.example
git commit -m "chore(chat): retrieval floor tuned for text-embedding-3-small"
```

### Task 6: Red-team run and close-out

**Files:**
- Modify: `evals/redteam/README.md` (results table)
- Modify: `docs/roadmap.md:126` (and the matching status table row if one exists)

- [ ] **Step 1: Run the suite**

```bash
npx promptfoo eval -c evals/redteam/promptfooconfig.yaml
```
Expected: structural 9/9. Record behaviour score.

- [ ] **Step 2: Checkpoint — decision point**

Bring structural/behaviour results to the user next to the llama3.2:3b baseline (9/9, 11/16). The user decides whether a lower behaviour score blocks merge (and whether system-prompt changes are allowed), and whether to run the judged slice with a paid OpenAI grader. Do what they decide; any prompt change is its own commit with `go test ./...` passing.

- [ ] **Step 3: Record results and mark done**

Add a row `| 2026-10-30 | gpt-4o-mini | <structural> | <behaviour> | <judged or not run> |` to `evals/redteam/README.md`. In `docs/roadmap.md` mark "Hosted OpenAI-compatible provider in place of Ollama" done as OpenAI `gpt-4o-mini`; leave "with a spend cap" as done only if the user confirmed the dev budget, prod cap stays a go-live item.

- [ ] **Step 4: Full check and commit**

```bash
cd services/chat-service && go test ./... && cd ../../frontend && npm test && npm run lint && npm run build
git add evals/redteam/README.md docs/roadmap.md
git commit -m "docs: red-team baseline on gpt-4o-mini; hosted provider done"
```
