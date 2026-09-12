# Retrieval-Augmented Chat over Qdrant Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace `chat-service`'s whole-corpus system prompt with real retrieval — `corpus/background.md` and this repository's own source become Qdrant points, and each turn grounds the reply in the passages that answer it.

**Architecture:** A new `chat-service/internal/rag` package holds the pieces both paths share — embeddings, the Qdrant HTTP client, chunkers, the source walk, and the ingest run. A new `chat-service/cmd/ingest` binary imports it to build the index and exits. `chat-service` imports it to search, wrapping the existing `OpenAIResponder` in a `RetrievingResponder` that assembles a per-turn system prompt and delegates. Nothing above the `Responder` seam changes: `proto/chat.proto`, `chatServer.Chat`, `envoy.yaml`, and every frontend file stay as they are.

**Tech Stack:** Go 1.26.3, stdlib `net/http` + `encoding/json` + `go/parser`/`go/ast` (no new module downloads), `github.com/google/uuid` promoted from indirect to direct, `promauto` for metrics, `httptest` for tests, Qdrant and Ollama in Docker Compose.

**Spec:** `docs/superpowers/specs/2026-09-08-rag-retrieval-design.md`. Read it before Task 1. Where this plan and the spec disagree, the four addenda below are the deliberate deviations; everything else follows the spec.

## Global Constraints

- Go module is `chat-service`, Go `1.26.3`. Each service directory is its own module — `cd chat-service` before any `go` command; there is no workspace.
- **No new module downloads.** The only `go.mod` change permitted is promoting `github.com/google/uuid v1.6.0` from `// indirect` to a direct require — it is already in `go.sum`. Everything else is stdlib plus what is already required.
- **Tests must be hermetic.** No test may contact Qdrant, Ollama, or any hosted API. `cd chat-service && go test ./...` must pass with the whole compose stack down.
- **`chat.go`'s single-metrics-site invariant holds:** exactly one `chatStreamsTotal.Inc()` and one `chatStreamDuration.Observe()` per `Chat` call, both inside the existing single deferred func. Retrieval metrics are separate collectors and must not add a second Inc/Observe pair for those two.
- **Client disconnects are not errors.** `context.Canceled` must be returned unwrapped from every new code path so `classifyOutcome` maps it to `cancelled`, and must not increment `chat_retrieval_errors_total`.
- **Startup is strict, runtime is soft.** Qdrant unreachable, collection missing, dimension mismatch, or collection empty are all startup errors. The same failures mid-stream degrade to an "unavailable" system prompt and still stream a reply.
- **`RESPONDER=echo` stays dependency-free.** Plain `docker compose up` must boot with no Qdrant, no Ollama, no GPU, and no corpus mount. Retrieval is constructed only under `RESPONDER=llm`.
- **The source allowlist is a security boundary.** `.env` must never be indexed. Selection is allow-by-extension under allow-listed roots, never a denylist. The index is quoted back to unauthenticated visitors.
- Exact config defaults: `EMBEDDER=ollama`, `EMBED_BASE_URL=http://ollama:11434/v1`, `EMBED_MODEL=nomic-embed-text`, `EMBED_API_KEY=` (empty), `QDRANT_URL=http://qdrant:6333`, `RETRIEVAL_TOP_K=6`, `RETRIEVAL_MIN_SCORE=0.5`, `RETRIEVAL_BACKGROUND_FLOOR=2`.
- Collection name is **derived, never configured**: `corpus__<sanitised model>__<dims>`.
- `chat_retrieval_errors_total` `reason` label, complete set: `unreachable | decode_error | collection_missing | embed_failed`. A cancelled client must increment none of them.
- Generated protobuf code (`chat-service/chat/`) is not touched by this plan.
- `order-service` scaling must keep working: do not add a `container_name` or host port mapping to it, and do not touch its compose block.
- `prometheus/prometheus.yml` and `promtail/promtail-config.yml` relabel regexes are fully anchored — any name added there needs `.*` on both sides.

### Spec addenda decided during planning

1. **Shared code lives in `chat-service/internal/rag`, not at the module root.** The spec's file table puts `embed.go`, `qdrant.go`, `chunk.go` and `sources.go` beside `main.go` and says both paths share them. They cannot: the module root is `package main`, and `cmd/ingest` — also `package main` — cannot import it. An internal package is the only shape that satisfies "shared by both paths". `retrieve.go` stays at the root because it touches `Responder` and the root's promauto collectors; `rag` exports no metrics, so the root observes durations around calls into it and ingest ignores them.

2. **Embedding dimensions are probed, not configured.** The spec derives the collection name from `<dims>` but lists no `EMBED_DIMS`. `NewOpenAIEmbedder` embeds a one-token probe string at construction and caches the length. This costs one call at startup and one per ingest run, and it makes the derived name impossible to get wrong — the same reasoning the spec gives for deriving the name at all.

3. **The model name is sanitised into the collection name.** Model ids legally contain `:` and `/` (`llama3.2:3b`, a hosted `org/model`). Qdrant collection names are path segments. Every rune outside `[A-Za-z0-9_-]` becomes `_`.

4. **`OpenAIResponder` gains `withSystem`.** Its `System` is a construction-time field, but retrieval produces a different prompt every turn. `withSystem` returns a shallow copy with `System` replaced — the `*http.Client` is shared, so this is a five-field copy per turn. `RetrievingResponder` depends on the one-method `grounder` interface rather than the concrete type, so it is testable with a fake.

---

### Task 1: Qdrant and the embedding model in compose, with the wire contracts verified

This task runs first on purpose. Tasks 2 and 3 assert against captured JSON, and two assumptions have not been confirmed: Ollama's `/v1/embeddings` response shape, and which Qdrant search endpoint the pinned image serves. Verify the wire format before writing decoders, so the test constants are recorded rather than transcribed from documentation.

**Files:**
- Modify: `docker-compose.yml` (new `qdrant` service, new named volume, second Ollama pull)
- Modify: `prometheus/prometheus.yml` (scrape `qdrant`)

**Interfaces:**
- Consumes: nothing.
- Produces: `http://qdrant:6333` reachable on `micro-network`; `nomic-embed-text` present in the `ollama-models` volume; and a recorded transcript of real request/response JSON that Tasks 2 and 3 must match.

- [ ] **Step 1: Add the `qdrant` service to `docker-compose.yml`**

Place it directly after the `ollama` block so the retrieval dependencies read together:

```yaml
  qdrant:
    image: qdrant/qdrant:v1.12.4
    container_name: qdrant
    # Same profile as ollama: retrieval is implied by RESPONDER=llm, and the
    # tracked default (echo) must still boot with neither container.
    profiles:
      - llm
    # No published host port, for the same reason as ollama: the REST API is
    # unauthenticated. Use `docker compose exec qdrant ...` to debug.
    volumes:
      - qdrant-storage:/qdrant/storage
    networks:
      - micro-network
    healthcheck:
      # /readyz is the readiness endpoint; / answers before storage is loaded.
      test: ["CMD-SHELL", "wget -qO- http://localhost:6333/readyz || exit 1"]
      interval: 10s
      timeout: 5s
      retries: 12
      start_period: 30s
```

Add the volume under the existing `volumes:` block:

```yaml
volumes:
  mongo-data:
  ollama-models:
  qdrant-storage:
```

- [ ] **Step 2: Pull the embedding model in the Ollama entrypoint**

In the `ollama` service's `entrypoint`, add a second pull immediately after the existing one, and extend the healthcheck to cover both. Replace the existing pull line and healthcheck with:

```yaml
        # Exit loudly on a failed pull. Staying up would leave the container
        # running but never healthy, so `up` would block for start_period plus
        # 60 retries — ~20 minutes — with the cause buried in the logs.
        ollama pull llama3.2:3b || exit 1
        # NOTE: duplicated in chat-service/.env as EMBED_MODEL. Change both
        # together, or ingest fails with reason="model_missing".
        ollama pull nomic-embed-text || exit 1
        wait $$pid
    # Healthy only once both models are actually pulled, not merely when the
    # API answers — otherwise chat-service starts and 404s on its first turn.
    healthcheck:
      test: ["CMD-SHELL", "ollama list | grep -q llama3.2:3b && ollama list | grep -q nomic-embed-text"]
      interval: 10s
      timeout: 5s
      retries: 60
      start_period: 600s
```

- [ ] **Step 3: Make `chat-service` wait for Qdrant**

Add to the `chat-service` service's `depends_on`, alongside the existing `ollama` entry:

```yaml
      qdrant:
        condition: service_healthy
        # Same reasoning as ollama: without the llm profile qdrant never
        # starts, and the echo default must not wait for it.
        required: false
```

- [ ] **Step 4: Scrape Qdrant's metrics**

Append to `prometheus/prometheus.yml`:

```yaml
  - job_name: qdrant
    static_configs:
      - targets: ['qdrant:6333']
```

Qdrant serves `/metrics` at the default path, so no `metrics_path` override is needed. A static target is correct here for the same reason `chat-service` uses one: the service has a `container_name` and is not scaled.

- [ ] **Step 5: Bring the stack up and record the real embedding contract**

```bash
docker compose --profile llm up -d ollama qdrant
docker compose exec ollama ollama list
```

Expected: both `llama3.2:3b` and `nomic-embed-text` listed. Then, from a container on the network:

```bash
docker compose exec ollama curl -s http://localhost:11434/v1/embeddings \
  -H 'Content-Type: application/json' \
  -d '{"model":"nomic-embed-text","input":["hello","world"]}' \
  | head -c 400
```

Record the answers to these three questions in your notes — Task 2's tests encode them:
1. Is the top-level array key `data`, and does each element carry `embedding` and `index`?
2. Are the vectors returned in request order, or must `index` be used to reorder?
3. What is `len(data[0].embedding)`? It must be 768.

- [ ] **Step 6: Record the real Qdrant contract**

```bash
docker compose exec qdrant sh -c '
  curl -s -X PUT http://localhost:6333/collections/probe \
    -H "Content-Type: application/json" \
    -d "{\"vectors\":{\"size\":4,\"distance\":\"Cosine\"}}"
  echo
  curl -s -X PUT "http://localhost:6333/collections/probe/points?wait=true" \
    -H "Content-Type: application/json" \
    -d "{\"points\":[{\"id\":\"3fa85f64-5717-4562-b3fc-2c963f66afa6\",\"vector\":[0.1,0.2,0.3,0.4],\"payload\":{\"kind\":\"background\",\"text\":\"probe\"}}]}"
  echo
  curl -s -X POST http://localhost:6333/collections/probe/points/search \
    -H "Content-Type: application/json" \
    -d "{\"vector\":[0.1,0.2,0.3,0.4],\"limit\":1,\"with_payload\":true,\"filter\":{\"must\":[{\"key\":\"kind\",\"match\":{\"value\":\"background\"}}]}}"
  echo
  curl -s http://localhost:6333/collections/probe
  echo
'
```

Record:
1. Does `POST /points/search` answer, or does this image require `POST /points/query`? If it requires `/points/query`, note the response shape difference (`result.points[]` rather than `result[]`) — Task 3 must match whichever this image actually serves.
2. Where does collection info report the vector size? Expected `result.config.params.vectors.size`.
3. Where does it report the point count? Expected `result.points_count`.

Clean up: `curl -s -X DELETE http://localhost:6333/collections/probe`.

- [ ] **Step 7: Commit**

```bash
git add docker-compose.yml prometheus/prometheus.yml
git commit -m "feat(rag): add qdrant and the embedding model to compose

Both sit behind the llm profile with no published host port: the REST and
inference APIs are unauthenticated, and the tracked echo default must still
boot without either container."
```

---

### Task 2: `rag.Embedder` and the OpenAI-shaped implementation

**Files:**
- Create: `chat-service/internal/rag/embed.go`
- Test: `chat-service/internal/rag/embed_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Embedder interface { Embed(ctx context.Context, texts []string) ([][]float32, error); Dims() int; ModelID() string }`
  - `func NewOpenAIEmbedder(ctx context.Context, baseURL, model, apiKey string, client *http.Client) (*OpenAIEmbedder, error)`
  - `func CollectionName(e Embedder) string`
  - `const EmbedBatchSize = 32`

- [ ] **Step 1: Write the failing tests**

Create `chat-service/internal/rag/embed_test.go`:

```go
package rag

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// embedServer answers /embeddings with a vector per input. dims sets the
// vector length so a test can simulate a model of any size.
func embedServer(t *testing.T, dims int, seen *[][]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("path = %q, want /embeddings", r.URL.Path)
		}
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decoding request: %v", err)
		}
		if seen != nil {
			*seen = append(*seen, req.Input)
		}
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		out := struct {
			Data []item `json:"data"`
		}{}
		// Reverse order on the wire: the client must reorder by index, not
		// trust arrival order.
		for i := len(req.Input) - 1; i >= 0; i-- {
			vec := make([]float32, dims)
			vec[0] = float32(i)
			out.Data = append(out.Data, item{Embedding: vec, Index: i})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	}))
}

// A mismatched vector order silently pairs every chunk with another chunk's
// vector: the index builds, searches, and returns confident nonsense.
func TestEmbedReordersByIndex(t *testing.T) {
	srv := embedServer(t, 768, nil)
	defer srv.Close()

	e, err := NewOpenAIEmbedder(context.Background(), srv.URL, "nomic-embed-text", "", srv.Client())
	if err != nil {
		t.Fatalf("NewOpenAIEmbedder() error = %v", err)
	}

	got, err := e.Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(vectors) = %d, want 3", len(got))
	}
	for i, vec := range got {
		if vec[0] != float32(i) {
			t.Errorf("vectors[%d][0] = %v, want %v — vectors were not reordered by index", i, vec[0], float32(i))
		}
	}
}

// Dims is probed once at construction. Re-probing per call would add a round
// trip to every ingest batch and every chat turn.
func TestDimsProbedOnceAtConstruction(t *testing.T) {
	var seen [][]string
	srv := embedServer(t, 768, &seen)
	defer srv.Close()

	e, err := NewOpenAIEmbedder(context.Background(), srv.URL, "nomic-embed-text", "", srv.Client())
	if err != nil {
		t.Fatalf("NewOpenAIEmbedder() error = %v", err)
	}
	if e.Dims() != 768 {
		t.Errorf("Dims() = %d, want 768", e.Dims())
	}
	if _, err := e.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
	if len(seen) != 2 {
		t.Errorf("request count = %d, want 2 (one probe, one Embed)", len(seen))
	}
}

// A batch larger than the provider accepts fails the whole ingest run. Split
// it here rather than discovering the provider's cap in production.
func TestEmbedSplitsIntoBatches(t *testing.T) {
	var seen [][]string
	srv := embedServer(t, 8, &seen)
	defer srv.Close()

	e, err := NewOpenAIEmbedder(context.Background(), srv.URL, "m", "", srv.Client())
	if err != nil {
		t.Fatalf("NewOpenAIEmbedder() error = %v", err)
	}
	seen = nil

	texts := make([]string, EmbedBatchSize+1)
	for i := range texts {
		texts[i] = "chunk"
	}
	got, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
	if len(got) != len(texts) {
		t.Errorf("len(vectors) = %d, want %d", len(got), len(texts))
	}
	if len(seen) != 2 {
		t.Fatalf("request count = %d, want 2 batches", len(seen))
	}
	if len(seen[0]) != EmbedBatchSize || len(seen[1]) != 1 {
		t.Errorf("batch sizes = %d, %d; want %d, 1", len(seen[0]), len(seen[1]), EmbedBatchSize)
	}
}

// Same dimension and a different model does not error at search time — it
// returns plausible scores over a space built by another model. The name is
// the only thing standing between that and a wrong answer.
func TestCollectionNameSanitisesModelID(t *testing.T) {
	tests := []struct {
		model string
		dims  int
		want  string
	}{
		{"nomic-embed-text", 768, "corpus__nomic-embed-text__768"},
		{"llama3.2:3b", 4096, "corpus__llama3_2_3b__4096"},
		{"openai/text-embedding-3-small", 1536, "corpus__openai_text-embedding-3-small__1536"},
	}
	for _, tt := range tests {
		got := CollectionName(fakeEmbedder{model: tt.model, dims: tt.dims})
		if got != tt.want {
			t.Errorf("CollectionName(%q, %d) = %q, want %q", tt.model, tt.dims, got, tt.want)
		}
	}
}

type fakeEmbedder struct {
	model string
	dims  int
	vec   []float32
	err   error
}

func (f fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i := range out {
		if f.vec != nil {
			out[i] = f.vec
			continue
		}
		out[i] = make([]float32, f.dims)
	}
	return out, nil
}
func (f fakeEmbedder) Dims() int      { return f.dims }
func (f fakeEmbedder) ModelID() string { return f.model }

// An unreachable embedder must not look like a decode failure: the two point
// an operator at different containers.
func TestEmbedUnreachable(t *testing.T) {
	srv := embedServer(t, 8, nil)
	e, err := NewOpenAIEmbedder(context.Background(), srv.URL, "m", "", srv.Client())
	if err != nil {
		t.Fatalf("NewOpenAIEmbedder() error = %v", err)
	}
	srv.Close()

	if _, err := e.Embed(context.Background(), []string{"a"}); err == nil {
		t.Error("Embed() error = nil, want an error against a closed server")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd chat-service && go test ./internal/rag/ -run 'TestEmbed|TestDims|TestCollectionName' -v
```

Expected: FAIL — `no required module provides package chat-service/internal/rag` or undefined symbols.

- [ ] **Step 3: Write the implementation**

Create `chat-service/internal/rag/embed.go`:

```go
// Package rag holds the retrieval pieces shared by the chat service and the
// ingest command. It exports no metrics: the chat service observes durations
// around these calls, and ingest prints a summary instead.
package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// EmbedBatchSize bounds one request's input array. Ollama accepts more, but a
// hosted provider's cap is the binding constraint and is not discoverable.
const EmbedBatchSize = 32

// Embedder turns text into vectors. Dims and ModelID are what the collection
// name is derived from, so an implementation must report the values it
// actually produces, not the ones it was configured with.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dims() int
	ModelID() string
}

// OpenAIEmbedder speaks the OpenAI-compatible /embeddings API. Locally that is
// Ollama; a hosted embedder is a base URL and a key, no code change.
type OpenAIEmbedder struct {
	BaseURL string // no trailing slash
	Model   string
	APIKey  string // empty for a local Ollama; sent as a Bearer token when set
	Client  *http.Client

	dims int
}

// probeText is embedded once at construction to learn the model's dimension.
// Any non-empty string works; a short one keeps the probe cheap.
const probeText = "dimension probe"

// NewOpenAIEmbedder probes the model's dimension so the collection name can be
// derived rather than configured. A name built from a configured dimension is
// one typo away from searching a space another model wrote.
func NewOpenAIEmbedder(ctx context.Context, baseURL, model, apiKey string, client *http.Client) (*OpenAIEmbedder, error) {
	e := &OpenAIEmbedder{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		Model:   model,
		APIKey:  apiKey,
		Client:  client,
	}
	vecs, err := e.post(ctx, []string{probeText})
	if err != nil {
		return nil, fmt.Errorf("probing embedding dimension for model %q: %w", model, err)
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		return nil, fmt.Errorf("embedding model %q returned no usable probe vector", model)
	}
	e.dims = len(vecs[0])
	return e, nil
}

func (e *OpenAIEmbedder) Dims() int      { return e.dims }
func (e *OpenAIEmbedder) ModelID() string { return e.Model }

// Embed returns one vector per text, in the order given.
func (e *OpenAIEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += EmbedBatchSize {
		end := min(start+EmbedBatchSize, len(texts))
		vecs, err := e.post(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		if len(vecs) != end-start {
			return nil, fmt.Errorf("embedder returned %d vectors for %d inputs", len(vecs), end-start)
		}
		out = append(out, vecs...)
	}
	return out, nil
}

type embedItem struct {
	Embedding []float32 `json:"embedding"`
	Index     int       `json:"index"`
}

func (e *OpenAIEmbedder) post(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}{Model: e.Model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("encoding embeddings request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building embeddings request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.APIKey != "" {
		// Omitted when unset: a bare "Bearer " is a malformed credential.
		req.Header.Set("Authorization", "Bearer "+e.APIKey)
	}

	resp, err := e.Client.Do(req)
	if err != nil {
		// Cancellation arrives wrapped in a transport error. Return it bare so
		// the caller's classifyOutcome sees a hangup, not a dead provider.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("embedder unreachable at %s: %w", e.BaseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("embedder returned HTTP %d for model %q: %s",
			resp.StatusCode, e.Model, strings.TrimSpace(string(detail)))
	}

	var out struct {
		Data []embedItem `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding embeddings response: %w", err)
	}

	// Ordering is not guaranteed by the API. Trusting arrival order would pair
	// each chunk with another chunk's vector — an index that searches cleanly
	// and answers wrongly.
	sort.Slice(out.Data, func(i, j int) bool { return out.Data[i].Index < out.Data[j].Index })
	vecs := make([][]float32, len(out.Data))
	for i, item := range out.Data {
		vecs[i] = item.Embedding
	}
	return vecs, nil
}

// CollectionName ties the collection to the space that wrote it. Two models of
// the same dimension do not error against each other's vectors; they return
// plausible scores for the wrong passages.
func CollectionName(e Embedder) string {
	return fmt.Sprintf("corpus__%s__%d", sanitiseModelID(e.ModelID()), e.Dims())
}

// Model ids legally contain ':' and '/'; a Qdrant collection name is a path
// segment.
func sanitiseModelID(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, id)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
cd chat-service && go test ./internal/rag/ -v
cd chat-service && go vet ./...
```

Expected: PASS.

- [ ] **Step 5: Reconcile with the Task 1 transcript**

Compare the recorded `/v1/embeddings` response against `embedItem`. If the real response omits `index`, the sort is a no-op on zero values and would collapse ordering — in that case replace the sort with a length check plus arrival order, and record why in a comment. Do not leave both.

- [ ] **Step 6: Commit**

```bash
git add chat-service/internal/rag/embed.go chat-service/internal/rag/embed_test.go
git commit -m "feat(rag): embedder over the OpenAI-compatible /embeddings API

Dimensions are probed at construction rather than configured, so the derived
collection name cannot disagree with the space it indexes."
```

---

### Task 3: The Qdrant client

**Files:**
- Create: `chat-service/internal/rag/qdrant.go`
- Test: `chat-service/internal/rag/qdrant_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `type Payload struct` with fields `Kind, Text, Source string; Line int; Symbol, Heading, ContentHash, RunID string`
  - `type Point struct { ID string; Vector []float32; Payload Payload }`
  - `type Hit struct { ID string; Score float32; Payload Payload }`
  - `type Store struct { BaseURL, Collection string; HTTP *http.Client }`
  - `func (s *Store) EnsureCollection(ctx context.Context, dims int) error`
  - `func (s *Store) Info(ctx context.Context) (dims, points int, err error)`
  - `func (s *Store) Upsert(ctx context.Context, pts []Point) error`
  - `func (s *Store) Search(ctx context.Context, vec []float32, limit int, kind string) ([]Hit, error)` — `kind == ""` searches unfiltered
  - `func (s *Store) ScrollAll(ctx context.Context, fn func(id string, p Payload) error) error`
  - `func (s *Store) StampRun(ctx context.Context, sources []string, runID string) error`
  - `func (s *Store) DeleteOtherRuns(ctx context.Context, runID string) error`
  - `var ErrCollectionMissing = errors.New(...)`

- [ ] **Step 1: Write the failing tests**

Create `chat-service/internal/rag/qdrant_test.go`:

```go
package rag

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newStore(t *testing.T, h http.HandlerFunc) (*Store, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	return &Store{BaseURL: srv.URL, Collection: "corpus__m__4", HTTP: srv.Client()}, srv.Close
}

// A missing collection must be distinguishable from an unreachable Qdrant:
// one is "run ingest", the other is "start the container".
func TestInfoReportsMissingCollection(t *testing.T) {
	s, done := newStore(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	defer done()

	if _, _, err := s.Info(context.Background()); !errors.Is(err, ErrCollectionMissing) {
		t.Errorf("Info() error = %v, want ErrCollectionMissing", err)
	}
}

// A collection built by a 768-dim model and searched with a 1536-dim vector
// is a startup error. Qdrant would reject every search at runtime instead.
func TestInfoReportsDimsAndCount(t *testing.T) {
	s, done := newStore(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"result":{"points_count":412,"config":{"params":{"vectors":{"size":768,"distance":"Cosine"}}}}}`))
	})
	defer done()

	dims, points, err := s.Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if dims != 768 || points != 412 {
		t.Errorf("Info() = (%d, %d), want (768, 412)", dims, points)
	}
}

// The background quota is what keeps "who are you?" answerable. If the filter
// is dropped, the quota silently becomes a second unfiltered search.
func TestSearchSendsKindFilter(t *testing.T) {
	var body map[string]any
	s, done := newStore(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"result":[{"id":"p1","score":0.81,"payload":{"kind":"background","text":"t","source":"corpus/background.md"}}]}`))
	})
	defer done()

	hits, err := s.Search(context.Background(), []float32{1, 2, 3, 4}, 2, "background")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(hits) != 1 || hits[0].Score != 0.81 || hits[0].Payload.Kind != "background" {
		t.Fatalf("hits = %+v, want one background hit scoring 0.81", hits)
	}

	filter, ok := body["filter"].(map[string]any)
	if !ok {
		t.Fatalf("request had no filter: %v", body)
	}
	if !strings.Contains(mustJSON(t, filter), `"value":"background"`) {
		t.Errorf("filter = %s, want a kind=background match", mustJSON(t, filter))
	}
}

// An unfiltered search must send no filter at all. An empty filter object is
// not the same request and some versions reject it.
func TestSearchOmitsFilterWhenKindEmpty(t *testing.T) {
	var body map[string]any
	s, done := newStore(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"result":[]}`))
	})
	defer done()

	if _, err := s.Search(context.Background(), []float32{1, 2, 3, 4}, 6, ""); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if _, present := body["filter"]; present {
		t.Errorf("request sent a filter for an unfiltered search: %v", body)
	}
}

// Scroll paginates. Reading only the first page makes the sweep believe every
// file past the page boundary was deleted.
func TestScrollAllFollowsPages(t *testing.T) {
	page := 0
	s, done := newStore(t, func(w http.ResponseWriter, r *http.Request) {
		page++
		if page == 1 {
			w.Write([]byte(`{"result":{"points":[{"id":"a","payload":{"source":"x.go","content_hash":"h1"}}],"next_page_offset":"a"}}`))
			return
		}
		w.Write([]byte(`{"result":{"points":[{"id":"b","payload":{"source":"y.go","content_hash":"h2"}}],"next_page_offset":null}}`))
	})
	defer done()

	var ids []string
	if err := s.ScrollAll(context.Background(), func(id string, p Payload) error {
		ids = append(ids, id)
		return nil
	}); err != nil {
		t.Fatalf("ScrollAll() error = %v", err)
	}
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Errorf("ids = %v, want [a b] across two pages", ids)
	}
}

// The sweep deletes by run_id. Getting the filter polarity wrong deletes the
// run that just succeeded and leaves the orphans.
func TestDeleteOtherRunsFiltersOnMustNot(t *testing.T) {
	var raw string
	s, done := newStore(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		raw = string(b)
		w.Write([]byte(`{"result":{"status":"completed"}}`))
	})
	defer done()

	if err := s.DeleteOtherRuns(context.Background(), "run-7"); err != nil {
		t.Fatalf("DeleteOtherRuns() error = %v", err)
	}
	if !strings.Contains(raw, "must_not") || !strings.Contains(raw, "run-7") {
		t.Errorf("delete body = %s, want a must_not match on run_id run-7", raw)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	return string(b)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd chat-service && go test ./internal/rag/ -run TestSearch -v
```

Expected: FAIL — undefined `Store`, `ErrCollectionMissing`.

- [ ] **Step 3: Write the implementation**

Create `chat-service/internal/rag/qdrant.go`. Use whichever search endpoint Task 1 Step 6 confirmed; the code below assumes `POST /points/search`.

```go
package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ErrCollectionMissing separates "run ingest" from "start the container".
// Both are startup errors; only one is fixed by bringing Qdrant up.
var ErrCollectionMissing = errors.New("qdrant collection does not exist")

// scrollPageSize bounds one scroll response. The sweep reads the whole
// collection, which is thousands of points, not millions.
const scrollPageSize = 256

// Payload is what a point carries back to the prompt. The json tags are the
// stored field names — ingest and search must not disagree about them.
type Payload struct {
	Kind        string `json:"kind"` // background | source | docs
	Text        string `json:"text"`
	Source      string `json:"source"`
	Line        int    `json:"line"`
	Symbol      string `json:"symbol,omitempty"`
	Heading     string `json:"heading,omitempty"`
	ContentHash string `json:"content_hash"`
	RunID       string `json:"run_id"`
}

type Point struct {
	ID      string    `json:"id"`
	Vector  []float32 `json:"vector"`
	Payload Payload   `json:"payload"`
}

type Hit struct {
	ID      string
	Score   float32
	Payload Payload
}

// Store is the Qdrant REST client. Collection is derived by CollectionName and
// is never taken from configuration.
type Store struct {
	BaseURL    string // no trailing slash
	Collection string
	HTTP       *http.Client
}

func (s *Store) url(suffix string) string {
	return fmt.Sprintf("%s/collections/%s%s", strings.TrimSuffix(s.BaseURL, "/"), s.Collection, suffix)
}

// do sends a request and decodes into out. A 404 becomes ErrCollectionMissing
// so callers do not have to parse status codes.
func (s *Store) do(ctx context.Context, method, url string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encoding qdrant request: %w", err)
		}
		body = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return fmt.Errorf("building qdrant request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.HTTP.Do(req)
	if err != nil {
		// Bare, so a cancelled chat stream is classified as a hangup.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("qdrant unreachable at %s: %w", s.BaseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", ErrCollectionMissing, s.Collection)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("qdrant returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding qdrant response: %w", err)
	}
	return nil
}

// EnsureCollection creates the collection if it is absent and returns a
// dimension mismatch as an error rather than recreating: recreating would
// silently discard an index another embedder is still serving from.
func (s *Store) EnsureCollection(ctx context.Context, dims int) error {
	switch existing, _, err := s.Info(ctx); {
	case err == nil:
		if existing != dims {
			return fmt.Errorf("collection %s has %d dimensions, embedder produces %d", s.Collection, existing, dims)
		}
		return nil
	case errors.Is(err, ErrCollectionMissing):
	default:
		return err
	}

	return s.do(ctx, http.MethodPut, s.url(""), map[string]any{
		"vectors": map[string]any{"size": dims, "distance": "Cosine"},
	}, nil)
}

func (s *Store) Info(ctx context.Context) (dims, points int, err error) {
	var out struct {
		Result struct {
			PointsCount int `json:"points_count"`
			Config      struct {
				Params struct {
					Vectors struct {
						Size int `json:"size"`
					} `json:"vectors"`
				} `json:"params"`
			} `json:"config"`
		} `json:"result"`
	}
	if err := s.do(ctx, http.MethodGet, s.url(""), nil, &out); err != nil {
		return 0, 0, err
	}
	return out.Result.Config.Params.Vectors.Size, out.Result.PointsCount, nil
}

// Upsert overwrites by ID. Deterministic IDs are what make a re-run an
// overwrite instead of an append.
func (s *Store) Upsert(ctx context.Context, pts []Point) error {
	if len(pts) == 0 {
		return nil
	}
	return s.do(ctx, http.MethodPut, s.url("/points?wait=true"),
		map[string]any{"points": pts}, nil)
}

// Search returns hits ordered by score. kind == "" searches unfiltered; any
// other value restricts to that payload kind.
func (s *Store) Search(ctx context.Context, vec []float32, limit int, kind string) ([]Hit, error) {
	req := map[string]any{
		"vector":       vec,
		"limit":        limit,
		"with_payload": true,
	}
	// Sent only when filtering: an empty filter object is a different request.
	if kind != "" {
		req["filter"] = kindFilter(kind)
	}

	var out struct {
		Result []struct {
			ID      any     `json:"id"`
			Score   float32 `json:"score"`
			Payload Payload `json:"payload"`
		} `json:"result"`
	}
	if err := s.do(ctx, http.MethodPost, s.url("/points/search"), req, &out); err != nil {
		return nil, err
	}

	hits := make([]Hit, 0, len(out.Result))
	for _, r := range out.Result {
		hits = append(hits, Hit{ID: fmt.Sprint(r.ID), Score: r.Score, Payload: r.Payload})
	}
	return hits, nil
}

func kindFilter(kind string) map[string]any {
	return map[string]any{
		"must": []any{map[string]any{
			"key":   "kind",
			"match": map[string]any{"value": kind},
		}},
	}
}

// ScrollAll walks every point. The sweep needs the whole collection, so a
// caller that stops at the first page would treat the remainder as deleted.
func (s *Store) ScrollAll(ctx context.Context, fn func(id string, p Payload) error) error {
	var offset any
	for {
		req := map[string]any{
			"limit":        scrollPageSize,
			"with_payload": true,
			"with_vector":  false,
		}
		if offset != nil {
			req["offset"] = offset
		}

		var out struct {
			Result struct {
				Points []struct {
					ID      any     `json:"id"`
					Payload Payload `json:"payload"`
				} `json:"points"`
				NextPageOffset any `json:"next_page_offset"`
			} `json:"result"`
		}
		if err := s.do(ctx, http.MethodPost, s.url("/points/scroll"), req, &out); err != nil {
			return err
		}

		for _, p := range out.Result.Points {
			if err := fn(fmt.Sprint(p.ID), p.Payload); err != nil {
				return err
			}
		}
		if out.Result.NextPageOffset == nil {
			return nil
		}
		offset = out.Result.NextPageOffset
	}
}

// StampRun restamps unchanged files' points with the current run. Without it
// the sweep deletes exactly the files that did not need re-embedding.
func (s *Store) StampRun(ctx context.Context, sources []string, runID string) error {
	if len(sources) == 0 {
		return nil
	}
	should := make([]any, 0, len(sources))
	for _, src := range sources {
		should = append(should, map[string]any{
			"key":   "source",
			"match": map[string]any{"value": src},
		})
	}
	return s.do(ctx, http.MethodPost, s.url("/points/payload?wait=true"), map[string]any{
		"payload": map[string]any{"run_id": runID},
		"filter":  map[string]any{"should": should},
	}, nil)
}

// DeleteOtherRuns removes every point the current run did not write or stamp.
// Callers must not reach here after a failed walk: unvisited sources are
// indistinguishable from deleted ones.
func (s *Store) DeleteOtherRuns(ctx context.Context, runID string) error {
	return s.do(ctx, http.MethodPost, s.url("/points/delete?wait=true"), map[string]any{
		"filter": map[string]any{
			"must_not": []any{map[string]any{
				"key":   "run_id",
				"match": map[string]any{"value": runID},
			}},
		},
	}, nil)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
cd chat-service && go test ./internal/rag/ -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add chat-service/internal/rag/qdrant.go chat-service/internal/rag/qdrant_test.go
git commit -m "feat(rag): qdrant REST client

Scroll paginates and the sweep filters on must_not run_id: reading one page or
inverting that filter deletes the run that just succeeded."
```

---

### Task 4: Chunkers

**Files:**
- Create: `chat-service/internal/rag/chunk.go`
- Test: `chat-service/internal/rag/chunk_test.go`
- Test fixtures: `chat-service/internal/rag/testdata/sample.md`, `chat-service/internal/rag/testdata/sample.go.txt`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `const ChunkerVersion = "1"`
  - `type Chunk struct { Text string; Line int; Symbol string; Heading string }`
  - `func ChunkMarkdown(src string) []Chunk`
  - `func ChunkGo(filename, src string) ([]Chunk, error)`

`.go.txt` rather than `.go` for the fixture: a `.go` file under `testdata/` is ignored by the toolchain, but a broken-on-purpose one is still confusing to read, and the fixture is parsed as text either way.

- [ ] **Step 1: Write the failing tests**

Create `chat-service/internal/rag/chunk_test.go`:

```go
package rag

import (
	"strings"
	"testing"
)

// The heading is the only context an isolated chunk has. Without it a
// paragraph about "the sweep" embeds with no idea what it sweeps.
func TestChunkMarkdownPrependsHeading(t *testing.T) {
	src := "# Top\n\nIntro paragraph.\n\n## Work history\n\nBackend engineer.\n"

	chunks := ChunkMarkdown(src)
	if len(chunks) < 2 {
		t.Fatalf("len(chunks) = %d, want at least 2", len(chunks))
	}

	var found bool
	for _, c := range chunks {
		if c.Heading != "Work history" {
			continue
		}
		found = true
		if !strings.HasPrefix(c.Text, "Work history") {
			t.Errorf("chunk text = %q, want the heading prepended", c.Text)
		}
		if !strings.Contains(c.Text, "Backend engineer.") {
			t.Errorf("chunk text = %q, want the section body", c.Text)
		}
	}
	if !found {
		t.Error("no chunk carried the Work history heading")
	}
}

// A sentence spanning a chunk boundary must stay retrievable from either
// side, or a question about it matches neither chunk well.
func TestChunkMarkdownOverlaps(t *testing.T) {
	var b strings.Builder
	b.WriteString("## Long\n\n")
	for i := 0; i < 60; i++ {
		b.WriteString("Sentence number with several words in it to burn tokens.\n\n")
	}

	chunks := ChunkMarkdown(b.String())
	if len(chunks) < 2 {
		t.Fatalf("len(chunks) = %d, want the section split", len(chunks))
	}

	tail := lastWords(chunks[0].Text, 5)
	if !strings.Contains(chunks[1].Text, tail) {
		t.Errorf("chunk[1] = %q...\ndoes not repeat chunk[0]'s tail %q", chunks[1].Text[:60], tail)
	}
}

// Chunks carry a line number so a citation points at the right place in the
// file. An always-zero line makes every source citation say :1.
func TestChunkMarkdownRecordsLine(t *testing.T) {
	src := "# Top\n\nIntro.\n\n## Second\n\nBody.\n"
	for _, c := range ChunkMarkdown(src) {
		if c.Heading == "Second" && c.Line != 5 {
			t.Errorf("Second section Line = %d, want 5", c.Line)
		}
	}
}

// A Go chunk without its package clause and doc comment embeds as an
// anonymous body: the retrievable context is in the parts around the code.
func TestChunkGoCarriesPackageAndDoc(t *testing.T) {
	src := `package carrier

import "fmt"

// Set writes one header. Trace context crosses RabbitMQ this way.
func Set(k, v string) { fmt.Println(k, v) }

type Table map[string]string
`
	chunks, err := ChunkGo("amqp_carrier.go", src)
	if err != nil {
		t.Fatalf("ChunkGo() error = %v", err)
	}

	bySymbol := map[string]Chunk{}
	for _, c := range chunks {
		bySymbol[c.Symbol] = c
	}

	set, ok := bySymbol["Set"]
	if !ok {
		t.Fatalf("no chunk for Set; got %v", keys(bySymbol))
	}
	if !strings.Contains(set.Text, "package carrier") {
		t.Errorf("Set chunk = %q, want the package clause", set.Text)
	}
	if !strings.Contains(set.Text, "Trace context crosses RabbitMQ") {
		t.Errorf("Set chunk = %q, want the doc comment", set.Text)
	}
	if set.Line != 6 {
		t.Errorf("Set chunk Line = %d, want 6", set.Line)
	}
	if _, ok := bySymbol["Table"]; !ok {
		t.Errorf("no chunk for the Table type; got %v", keys(bySymbol))
	}
}

// Imports are not answers. A chunk per import block crowds real declarations
// out of top_k with the least informative lines in the file.
func TestChunkGoSkipsImports(t *testing.T) {
	chunks, err := ChunkGo("x.go", "package p\n\nimport \"fmt\"\n\nvar A = fmt.Sprint\n")
	if err != nil {
		t.Fatalf("ChunkGo() error = %v", err)
	}
	for _, c := range chunks {
		if strings.HasPrefix(strings.TrimSpace(c.Text), "import") {
			t.Errorf("emitted an import chunk: %q", c.Text)
		}
	}
}

// A file that does not parse must not abort the run: one broken file should
// not leave the whole index stale.
func TestChunkGoReturnsErrorOnParseFailure(t *testing.T) {
	if _, err := ChunkGo("bad.go", "package p\nfunc ( {"); err == nil {
		t.Error("ChunkGo() error = nil, want a parse error")
	}
}

func lastWords(s string, n int) string {
	f := strings.Fields(s)
	if len(f) < n {
		return strings.Join(f, " ")
	}
	return strings.Join(f[len(f)-n:], " ")
}

func keys(m map[string]Chunk) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd chat-service && go test ./internal/rag/ -run TestChunk -v
```

Expected: FAIL — undefined `ChunkMarkdown`, `ChunkGo`.

- [ ] **Step 3: Write the implementation**

Create `chat-service/internal/rag/chunk.go`:

```go
package rag

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

// ChunkerVersion is hashed with the file bytes. Editing this file leaves file
// bytes identical, so a hash over bytes alone would skip everything and leave
// the index built by the previous chunker.
const ChunkerVersion = "1"

// Rough token budget per chunk and the overlap between neighbours. Tokens are
// approximated as words: precise counting would need the model's tokeniser for
// a bound that only has to be approximately right.
const (
	chunkTargetWords  = 300
	chunkOverlapWords = 50
)

type Chunk struct {
	Text    string
	Line    int    // 1-based, where this chunk starts in the file
	Symbol  string // Go only
	Heading string // markdown only
}

// ChunkMarkdown splits on headings, then packs each section to roughly
// chunkTargetWords with chunkOverlapWords of overlap.
func ChunkMarkdown(src string) []Chunk {
	var out []Chunk
	for _, sec := range splitSections(src) {
		for _, part := range packWords(sec.body, sec.line) {
			text := part.text
			if sec.heading != "" {
				// Prepended, not stored alongside: the embedding is of the
				// text, so context outside it does not reach the vector.
				text = sec.heading + "\n\n" + text
			}
			out = append(out, Chunk{Text: text, Line: part.line, Heading: sec.heading})
		}
	}
	return out
}

type section struct {
	heading string
	body    string
	line    int
}

func splitSections(src string) []section {
	lines := strings.Split(src, "\n")
	var (
		out  []section
		cur  = section{line: 1}
		body strings.Builder
	)
	flush := func() {
		if strings.TrimSpace(body.String()) == "" {
			return
		}
		cur.body = body.String()
		out = append(out, cur)
		body.Reset()
	}

	for i, line := range lines {
		if h, ok := headingText(line); ok {
			flush()
			// The heading's own line, so a citation points at the section.
			cur = section{heading: h, line: i + 1}
			continue
		}
		// Preamble before the first heading falls into cur, which keeps line 1.
		body.WriteString(line)
		body.WriteString("\n")
	}
	flush()
	return out
}

func headingText(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "#") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimLeft(trimmed, "#")), true
}

type part struct {
	text string
	line int
}

// packWords fills chunks to chunkTargetWords, carrying chunkOverlapWords from
// the previous chunk into the next.
func packWords(body string, startLine int) []part {
	words := strings.Fields(body)
	if len(words) == 0 {
		return nil
	}
	if len(words) <= chunkTargetWords {
		return []part{{text: strings.TrimSpace(body), line: startLine}}
	}

	var out []part
	for start := 0; start < len(words); start += chunkTargetWords - chunkOverlapWords {
		end := min(start+chunkTargetWords, len(words))
		out = append(out, part{text: strings.Join(words[start:end], " "), line: startLine})
		if end == len(words) {
			break
		}
	}
	return out
}

// ChunkGo chunks per top-level declaration, carrying the package clause and
// the declaration's doc comment. An error means the file is skipped, not that
// the run fails.
func ChunkGo(filename, src string) ([]Chunk, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", filename, err)
	}

	pkg := "package " + file.Name.Name
	lines := strings.Split(src, "\n")

	var out []Chunk
	for _, decl := range file.Decls {
		// Imports are not answers, and one chunk per import block would crowd
		// real declarations out of top_k.
		if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
			continue
		}

		start := fset.Position(declStart(decl)).Line
		end := fset.Position(decl.End()).Line
		if start < 1 || end > len(lines) {
			continue
		}

		body := strings.Join(lines[start-1:end], "\n")
		out = append(out, Chunk{
			Text:   pkg + "\n\n" + body,
			Line:   fset.Position(decl.Pos()).Line,
			Symbol: declSymbol(decl),
		})
	}
	return out, nil
}

// declStart includes the doc comment: the reasoning above a declaration is
// usually what a question about it matches on.
func declStart(decl ast.Decl) token.Pos {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Doc != nil {
			return d.Doc.Pos()
		}
	case *ast.GenDecl:
		if d.Doc != nil {
			return d.Doc.Pos()
		}
	}
	return decl.Pos()
}

// declSymbol names the chunk for the citation. A multi-name const or var block
// is named for its first entry; the whole block is one chunk either way.
func declSymbol(decl ast.Decl) string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv != nil && len(d.Recv.List) > 0 {
			return receiverName(d.Recv.List[0].Type) + "." + d.Name.Name
		}
		return d.Name.Name
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				return s.Name.Name
			case *ast.ValueSpec:
				if len(s.Names) > 0 {
					return s.Names[0].Name
				}
			}
		}
	}
	return ""
}

func receiverName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return receiverName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr: // generic receiver
		return receiverName(t.X)
	}
	return ""
}
```

Note on `TestChunkGoCarriesPackageAndDoc`: `Line` is `decl.Pos()`, the declaration itself, not the doc comment — a citation should point at the code. `Text` starts at `declStart`, which includes the doc. The test asserts `Line == 6`, the `func Set` line.

- [ ] **Step 4: Run the tests to verify they pass**

```bash
cd chat-service && go test ./internal/rag/ -run TestChunk -v
```

Expected: PASS. If `TestChunkMarkdownOverlaps` fails, the overlap arithmetic is wrong — `packWords` steps by `chunkTargetWords - chunkOverlapWords`, so consecutive chunks share exactly `chunkOverlapWords`.

- [ ] **Step 5: Commit**

```bash
git add chat-service/internal/rag/chunk.go chat-service/internal/rag/chunk_test.go
git commit -m "feat(rag): markdown and Go chunkers

Headings and package clauses travel inside the chunk text: an embedding only
sees what it is given, so context stored beside it never reaches the vector."
```

---

### Task 5: The allowlisted source walk

**Files:**
- Create: `chat-service/internal/rag/sources.go`
- Test: `chat-service/internal/rag/sources_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Source struct { Path string; Kind string }` — `Path` is repo-relative with forward slashes
  - `func Walk(root string) ([]Source, error)`

- [ ] **Step 1: Write the failing tests**

Create `chat-service/internal/rag/sources_test.go`:

```go
package rag

import (
	"os"
	"path/filepath"
	"testing"
)

// This is the security test for the whole feature. The index is quoted back to
// unauthenticated visitors; a selected .env is a credential leak, not a bug.
func TestWalkNeverSelectsSecrets(t *testing.T) {
	root := fixtureTree(t)

	got, err := Walk(root)
	if err != nil {
		t.Fatalf("Walk() error = %v", err)
	}
	for _, s := range got {
		switch s.Path {
		case ".env", "chat-service/.env", "chat-service/.env.local":
			t.Errorf("Walk() selected %q — the allowlist must exclude it", s.Path)
		}
	}
}

func TestWalkSelectsAllowedRootsAndExtensions(t *testing.T) {
	root := fixtureTree(t)

	got, err := Walk(root)
	if err != nil {
		t.Fatalf("Walk() error = %v", err)
	}
	kinds := map[string]string{}
	for _, s := range got {
		kinds[s.Path] = s.Kind
	}

	want := map[string]string{
		"corpus/background.md":                  "background",
		"chat-service/chat.go":                  "source",
		"frontend/src/api.ts":                   "source",
		"proto/chat.proto":                      "source",
		"docs/superpowers/specs/a-design.md":    "docs",
		"CLAUDE.md":                             "docs",
		"README.md":                             "docs",
	}
	for path, wantKind := range want {
		if kinds[path] != wantKind {
			t.Errorf("kind for %q = %q, want %q", path, kinds[path], wantKind)
		}
	}
}

// Generated protobuf, build output and tests would fill top_k with material
// nobody asks about.
func TestWalkSkipsGeneratedAndTests(t *testing.T) {
	root := fixtureTree(t)

	got, err := Walk(root)
	if err != nil {
		t.Fatalf("Walk() error = %v", err)
	}
	for _, s := range got {
		switch s.Path {
		case "chat-service/chat/chat.pb.go",
			"frontend/src/gen/chat_pb.ts",
			"frontend/node_modules/pkg/index.ts",
			"chat-service/chat_test.go",
			"docs/scalability-learning-plan.md":
			t.Errorf("Walk() selected %q, which must be skipped", s.Path)
		}
	}
}

// Paths become citations and point IDs. A backslash on Windows would produce
// a different ID for the same file than a Linux ingest run.
func TestWalkUsesForwardSlashes(t *testing.T) {
	root := fixtureTree(t)

	got, err := Walk(root)
	if err != nil {
		t.Fatalf("Walk() error = %v", err)
	}
	for _, s := range got {
		if filepath.Separator != '/' && filepath.ToSlash(s.Path) != s.Path {
			t.Errorf("path %q is not slash-separated", s.Path)
		}
	}
}

func fixtureTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := []string{
		".env",
		"chat-service/.env",
		"chat-service/.env.local",
		"corpus/background.md",
		"chat-service/chat.go",
		"chat-service/chat_test.go",
		"chat-service/chat/chat.pb.go",
		"frontend/src/api.ts",
		"frontend/src/gen/chat_pb.ts",
		"frontend/node_modules/pkg/index.ts",
		"proto/chat.proto",
		"docs/superpowers/specs/a-design.md",
		"docs/scalability-learning-plan.md",
		"CLAUDE.md",
		"README.md",
		"secrets.txt",
	}
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	return root
}
```

Note: `docs/scalability-learning-plan.md` sits under `docs/`, which is **not** an allowed root — only `docs/superpowers` is. That file is gitignored and local-only; indexing it would publish the roadmap.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd chat-service && go test ./internal/rag/ -run TestWalk -v
```

Expected: FAIL — undefined `Walk`.

- [ ] **Step 3: Write the implementation**

Create `chat-service/internal/rag/sources.go`:

```go
package rag

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Source is one indexable file, repo-relative with forward slashes. The path
// becomes both a citation and half of the point ID, so its shape must not vary
// by host OS.
type Source struct {
	Path string
	Kind string // background | source | docs
}

// allowedRoots is an allowlist, not a denylist. A denylist fails open on the
// file nobody thought of, and this index is quoted back to unauthenticated
// visitors. .env has no allowed extension, so no rule has to remember it.
var allowedRoots = []struct {
	prefix string
	kind   string
}{
	{"corpus", "background"},
	{"proto", "source"},
	{"order-service", "source"},
	{"chat-service", "source"},
	{"inventory-service", "source"},
	{"frontend/src", "source"},
	{"docs/superpowers", "docs"},
	{"CLAUDE.md", "docs"},
	{"README.md", "docs"},
}

var allowedExts = map[string]bool{
	".md": true, ".go": true, ".ts": true, ".tsx": true, ".proto": true,
}

// skipDirs are build output and generated protobuf. "orders" and "chat" are
// the committed generated packages named by their go_package.
var skipDirs = map[string]bool{
	"node_modules": true, "dist": true, "gen": true, "orders": true, "chat": true,
}

// Walk selects every file that sits under an allowed root, carries an allowed
// extension, and lies under no skipped directory.
func Walk(root string) ([]Source, error) {
	var out []Source

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}

		if d.IsDir() {
			if skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !allowedExts[strings.ToLower(path.Ext(rel))] {
			return nil
		}
		// Assertions retrieve poorly and crowd real code out of top_k.
		if strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		if kind, ok := rootKind(rel); ok {
			out = append(out, Source{Path: rel, Kind: kind})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking %s: %w", root, err)
	}
	return out, nil
}

// rootKind reports the kind for rel, or false if it is under no allowed root.
// A prefix match is on path segments: "corpus" must not match "corpusdump.md".
func rootKind(rel string) (string, bool) {
	for _, r := range allowedRoots {
		if rel == r.prefix || strings.HasPrefix(rel, r.prefix+"/") {
			return r.kind, true
		}
	}
	return "", false
}

// ReadSource reads one selected file. Kept here so callers never build a path
// from a Source themselves and reach outside root.
func ReadSource(root string, s Source) ([]byte, error) {
	return os.ReadFile(filepath.Join(root, filepath.FromSlash(s.Path)))
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
cd chat-service && go test ./internal/rag/ -run TestWalk -v
cd chat-service && go vet ./...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add chat-service/internal/rag/sources.go chat-service/internal/rag/sources_test.go
git commit -m "feat(rag): allowlisted source walk

Allow-by-extension under allow-listed roots. A denylist fails open on the file
nobody thought of, and this index is quoted to unauthenticated visitors."
```

---

### Task 6: The ingest run and its command

**Files:**
- Create: `chat-service/internal/rag/ingest.go`
- Create: `chat-service/cmd/ingest/main.go`
- Test: `chat-service/internal/rag/ingest_test.go`
- Modify: `chat-service/go.mod` (promote `github.com/google/uuid` to a direct require)

**Interfaces:**
- Consumes: `Embedder`, `Store`, `Walk`, `ReadSource`, `ChunkMarkdown`, `ChunkGo`, `ChunkerVersion`, `CollectionName`.
- Produces:
  - `type Options struct { Root string; Store *Store; Embedder Embedder; Full bool; Log func(format string, args ...any) }`
  - `type Stats struct { Scanned, SkippedFiles, EmbeddedChunks, EmbeddedFiles, Total int }` — no swept count: `DeleteOtherRuns` is a filtered delete and Qdrant does not report how many points it matched. The `Total` line is what shows the sweep's effect.
  - `func Ingest(ctx context.Context, opts Options) (Stats, error)`
  - `func PointID(source string, index int) string`

- [ ] **Step 1: Write the failing tests**

Create `chat-service/internal/rag/ingest_test.go`:

```go
package rag

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeQdrant is an in-memory stand-in: points by ID, plus a record of what the
// sweep did. Enough to assert idempotency without a container.
type fakeQdrant struct {
	mu      sync.Mutex
	points  map[string]Payload
	deletes int
}

func newFakeQdrant(t *testing.T) (*Store, *fakeQdrant, func()) {
	t.Helper()
	f := &fakeQdrant{points: map[string]Payload{}}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/points") && r.Method == http.MethodPut:
			var in struct{ Points []Point }
			json.NewDecoder(r.Body).Decode(&in)
			for _, p := range in.Points {
				f.points[p.ID] = p.Payload
			}
			w.Write([]byte(`{"result":{"status":"completed"}}`))

		case strings.HasSuffix(r.URL.Path, "/points/scroll"):
			type outPoint struct {
				ID      string  `json:"id"`
				Payload Payload `json:"payload"`
			}
			out := struct {
				Result struct {
					Points         []outPoint `json:"points"`
					NextPageOffset any        `json:"next_page_offset"`
				} `json:"result"`
			}{}
			for id, p := range f.points {
				out.Result.Points = append(out.Result.Points, outPoint{ID: id, Payload: p})
			}
			json.NewEncoder(w).Encode(out)

		case strings.HasSuffix(r.URL.Path, "/points/payload"):
			var in struct {
				Payload map[string]string `json:"payload"`
				Filter  struct {
					Should []struct {
						Match struct{ Value string } `json:"match"`
					} `json:"should"`
				} `json:"filter"`
			}
			json.NewDecoder(r.Body).Decode(&in)
			want := map[string]bool{}
			for _, s := range in.Filter.Should {
				want[s.Match.Value] = true
			}
			for id, p := range f.points {
				if want[p.Source] {
					p.RunID = in.Payload["run_id"]
					f.points[id] = p
				}
			}
			w.Write([]byte(`{"result":{"status":"completed"}}`))

		case strings.HasSuffix(r.URL.Path, "/points/delete"):
			var in struct {
				Filter struct {
					MustNot []struct {
						Match struct{ Value string } `json:"match"`
					} `json:"must_not"`
				} `json:"filter"`
			}
			json.NewDecoder(r.Body).Decode(&in)
			keep := in.Filter.MustNot[0].Match.Value
			for id, p := range f.points {
				if p.RunID != keep {
					delete(f.points, id)
					f.deletes++
				}
			}
			w.Write([]byte(`{"result":{"status":"completed"}}`))

		default: // collection info / create
			w.Write([]byte(`{"result":{"points_count":0,"config":{"params":{"vectors":{"size":8}}}}}`))
		}
	}))

	return &Store{BaseURL: srv.URL, Collection: "corpus__fake__8", HTTP: srv.Client()}, f, srv.Close
}

func ingestFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, root, "corpus/background.md", "# Background\n\nBackend engineer in Finland.\n")
	write(t, root, "chat-service/chat.go", "package main\n\n// A does a thing.\nfunc A() {}\n")
	write(t, root, "chat-service/.env", "SECRET=hunter2\n")
	return root
}

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func runIngest(t *testing.T, root string, store *Store, full bool) Stats {
	t.Helper()
	st, err := Ingest(context.Background(), Options{
		Root:     root,
		Store:    store,
		Embedder: fakeEmbedder{model: "fake", dims: 8},
		Full:     full,
		Log:      func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}
	return st
}

// Deterministic IDs are the whole basis of re-running ingest. If they drift,
// every run doubles the index and every search returns duplicates.
func TestIngestIsIdempotent(t *testing.T) {
	root := ingestFixture(t)
	store, fake, done := newFakeQdrant(t)
	defer done()

	first := runIngest(t, root, store, false)
	countAfterFirst := len(fake.points)
	if countAfterFirst == 0 {
		t.Fatal("first run indexed nothing")
	}

	second := runIngest(t, root, store, false)
	if len(fake.points) != countAfterFirst {
		t.Errorf("point count = %d after a second run, want %d", len(fake.points), countAfterFirst)
	}
	if second.EmbeddedChunks != 0 {
		t.Errorf("second run embedded %d chunks, want 0 — content hashes did not match", second.EmbeddedChunks)
	}
	if second.SkippedFiles != first.Scanned {
		t.Errorf("second run skipped %d of %d files, want all", second.SkippedFiles, first.Scanned)
	}
}

// A file edited from many chunks down to few leaves stale points that are
// still searchable and now say something the file no longer says.
func TestIngestSweepsOrphansFromShrunkFile(t *testing.T) {
	root := ingestFixture(t)
	store, fake, done := newFakeQdrant(t)
	defer done()

	var long strings.Builder
	long.WriteString("# Background\n\n")
	for i := 0; i < 80; i++ {
		long.WriteString("A sentence with quite a few words in it for bulk.\n\n")
	}
	write(t, root, "corpus/background.md", long.String())
	runIngest(t, root, store, false)
	before := len(fake.points)

	write(t, root, "corpus/background.md", "# Background\n\nShort now.\n")
	runIngest(t, root, store, false)

	if len(fake.points) >= before {
		t.Errorf("point count = %d after shrinking, want fewer than %d", len(fake.points), before)
	}
	for id, p := range fake.points {
		if p.Source == "corpus/background.md" && !strings.Contains(p.Text, "Short now") {
			t.Errorf("stale chunk survived the sweep: %s = %q", id, p.Text)
		}
	}
}

// A deleted file is never visited again, so nothing overwrites its points.
func TestIngestSweepsDeletedFile(t *testing.T) {
	root := ingestFixture(t)
	store, fake, done := newFakeQdrant(t)
	defer done()

	runIngest(t, root, store, false)
	if err := os.Remove(filepath.Join(root, "chat-service", "chat.go")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	runIngest(t, root, store, false)

	for id, p := range fake.points {
		if p.Source == "chat-service/chat.go" {
			t.Errorf("deleted file's point survived: %s", id)
		}
	}
}

// The unchanged-file restamp is easy to omit and its absence deletes exactly
// the files that did not need re-embedding.
func TestIngestRestampsUnchangedFiles(t *testing.T) {
	root := ingestFixture(t)
	store, fake, done := newFakeQdrant(t)
	defer done()

	runIngest(t, root, store, false)
	before := len(fake.points)
	runIngest(t, root, store, false)

	if len(fake.points) != before {
		t.Errorf("point count = %d after an unchanged re-run, want %d — unchanged files were not restamped", len(fake.points), before)
	}
}

// Same guarantee as the walk test, one layer up: nothing that reaches Qdrant
// may come from a file the allowlist excludes.
func TestIngestNeverStoresSecrets(t *testing.T) {
	root := ingestFixture(t)
	store, fake, done := newFakeQdrant(t)
	defer done()

	runIngest(t, root, store, false)
	for id, p := range fake.points {
		if strings.Contains(p.Text, "hunter2") || strings.HasSuffix(p.Source, ".env") {
			t.Errorf("point %s carries .env content: %+v", id, p)
		}
	}
}

// A run that failed partway must sweep nothing: its unvisited sources are
// indistinguishable from deleted ones.
func TestIngestDoesNotSweepAfterEmbedFailure(t *testing.T) {
	root := ingestFixture(t)
	store, fake, done := newFakeQdrant(t)
	defer done()

	runIngest(t, root, store, false)
	before := len(fake.points)

	write(t, root, "chat-service/chat.go", "package main\n\n// B does another thing.\nfunc B() {}\n")
	_, err := Ingest(context.Background(), Options{
		Root:     root,
		Store:    store,
		Embedder: fakeEmbedder{model: "fake", dims: 8, err: errContext},
		Log:      func(string, ...any) {},
	})
	if err == nil {
		t.Fatal("Ingest() error = nil, want the embedder failure surfaced")
	}
	if len(fake.points) != before {
		t.Errorf("point count = %d after a failed run, want %d untouched", len(fake.points), before)
	}
	if fake.deletes != 0 {
		t.Errorf("failed run deleted %d points, want 0", fake.deletes)
	}
}

var errContext = errStub("embedder down")

type errStub string

func (e errStub) Error() string { return string(e) }
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd chat-service && go test ./internal/rag/ -run TestIngest -v
```

Expected: FAIL — undefined `Ingest`, `Options`, `Stats`.

- [ ] **Step 3: Promote the uuid dependency**

```bash
cd chat-service && go get github.com/google/uuid@v1.6.0 && go mod tidy
git diff go.mod
```

Expected: `github.com/google/uuid v1.6.0` moves out of the `// indirect` block. `go.sum` must not gain a new module.

- [ ] **Step 4: Write the implementation**

Create `chat-service/internal/rag/ingest.go`:

```go
package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"

	"github.com/google/uuid"
)

// pointNamespace is a fixed UUIDv5 namespace. Changing it re-IDs every point,
// which turns the next run into a full re-index plus a full sweep.
var pointNamespace = uuid.MustParse("6f5b2f3c-1f2a-4b7e-9a0d-3c8e5f21ab90")

type Options struct {
	Root     string
	Store    *Store
	Embedder Embedder
	Full     bool // ignore content hashes and re-embed everything
	Log      func(format string, args ...any)
}

// Stats has no swept count: DeleteOtherRuns is a filtered delete and Qdrant
// does not report how many points it matched. Total shows the effect.
type Stats struct {
	Scanned        int
	SkippedFiles   int
	EmbeddedChunks int
	EmbeddedFiles  int
	Total          int
}

// PointID is deterministic, so a re-run overwrites rather than appends. Qdrant
// accepts only an unsigned integer or a UUID, so the readable form is hashed.
func PointID(source string, index int) string {
	return uuid.NewSHA1(pointNamespace, []byte(fmt.Sprintf("%s:%d", source, index))).String()
}

// contentHash covers ChunkerVersion as well as the bytes: editing a chunker
// leaves file bytes identical, and a hash over bytes alone would skip every
// file and leave the index built by the previous chunker.
func contentHash(body []byte) string {
	h := sha256.New()
	h.Write(body)
	h.Write([]byte(ChunkerVersion))
	return hex.EncodeToString(h.Sum(nil))
}

// Ingest indexes Root into Store. The sweep runs only after the walk completes
// successfully — a partial run's unvisited sources are indistinguishable from
// deleted ones.
func Ingest(ctx context.Context, opts Options) (Stats, error) {
	var stats Stats
	runID := uuid.NewString()

	if err := opts.Store.EnsureCollection(ctx, opts.Embedder.Dims()); err != nil {
		return stats, err
	}

	// One scroll pass rather than a query per file: the sweep needs the whole
	// collection anyway.
	indexed := map[string]string{} // source -> content_hash
	if err := opts.Store.ScrollAll(ctx, func(_ string, p Payload) error {
		indexed[p.Source] = p.ContentHash
		return nil
	}); err != nil {
		return stats, err
	}

	sources, err := Walk(opts.Root)
	if err != nil {
		return stats, err
	}
	stats.Scanned = len(sources)

	var unchanged []string
	for _, src := range sources {
		body, err := ReadSource(opts.Root, src)
		if err != nil {
			return stats, err
		}
		hash := contentHash(body)

		if !opts.Full && indexed[src.Path] == hash {
			stats.SkippedFiles++
			unchanged = append(unchanged, src.Path)
			continue
		}

		chunks, err := chunkFile(src.Path, string(body))
		if err != nil {
			// One unparseable file must not leave the whole index stale.
			opts.Log("  skipped %s: %v", src.Path, err)
			stats.SkippedFiles++
			unchanged = append(unchanged, src.Path)
			continue
		}
		if len(chunks) == 0 {
			continue
		}

		texts := make([]string, len(chunks))
		for i, c := range chunks {
			texts[i] = c.Text
		}
		vecs, err := opts.Embedder.Embed(ctx, texts)
		if err != nil {
			return stats, fmt.Errorf("embedding %s: %w", src.Path, err)
		}

		pts := make([]Point, len(chunks))
		for i, c := range chunks {
			pts[i] = Point{
				ID:     PointID(src.Path, i),
				Vector: vecs[i],
				Payload: Payload{
					Kind:        src.Kind,
					Text:        c.Text,
					Source:      src.Path,
					Line:        c.Line,
					Symbol:      c.Symbol,
					Heading:     c.Heading,
					ContentHash: hash,
					RunID:       runID,
				},
			}
		}
		if err := opts.Store.Upsert(ctx, pts); err != nil {
			return stats, err
		}
		stats.EmbeddedChunks += len(pts)
		stats.EmbeddedFiles++
	}

	// Before the delete, never after: skipping this deletes exactly the files
	// that did not need re-embedding.
	sort.Strings(unchanged)
	if err := opts.Store.StampRun(ctx, unchanged, runID); err != nil {
		return stats, err
	}
	if err := opts.Store.DeleteOtherRuns(ctx, runID); err != nil {
		return stats, err
	}

	_, total, err := opts.Store.Info(ctx)
	if err != nil {
		return stats, err
	}
	stats.Total = total
	return stats, nil
}

func chunkFile(relPath, body string) ([]Chunk, error) {
	switch path.Ext(relPath) {
	case ".go":
		return ChunkGo(path.Base(relPath), body)
	default:
		// Markdown chunking is prose-shaped and works acceptably on .ts, .tsx
		// and .proto. A per-language chunker for each is a later increment.
		return ChunkMarkdown(body), nil
	}
}
```

- [ ] **Step 5: Write the ingest command**

Create `chat-service/cmd/ingest/main.go`:

```go
// Command ingest builds the retrieval index and exits. Serving stays decoupled
// from indexing: indexing at startup would grow boot time with the corpus and
// re-embed on every restart.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"chat-service/internal/rag"
)

func main() {
	full := flag.Bool("full", false, "ignore content hashes and re-embed every file")
	root := flag.String("root", envOr("INGEST_ROOT", "/workspace"), "repository root to index")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	client := &http.Client{Timeout: 2 * time.Minute}

	embedder, err := rag.NewOpenAIEmbedder(ctx,
		envOr("EMBED_BASE_URL", "http://ollama:11434/v1"),
		envOr("EMBED_MODEL", "nomic-embed-text"),
		os.Getenv("EMBED_API_KEY"),
		client)
	if err != nil {
		fail(err)
	}

	store := &rag.Store{
		BaseURL:    envOr("QDRANT_URL", "http://qdrant:6333"),
		Collection: rag.CollectionName(embedder),
		HTTP:       client,
	}

	stats, err := rag.Ingest(ctx, rag.Options{
		Root:     *root,
		Store:    store,
		Embedder: embedder,
		Full:     *full,
		Log:      func(format string, args ...any) { fmt.Printf(format+"\n", args...) },
	})
	if err != nil {
		fail(err)
	}

	fmt.Printf("  scanned  %4d files\n", stats.Scanned)
	fmt.Printf("  skipped  %4d unchanged\n", stats.SkippedFiles)
	fmt.Printf("  embedded %4d chunks from %d files\n", stats.EmbeddedChunks, stats.EmbeddedFiles)
	fmt.Printf("  total    %4d points in %s\n", stats.Total, store.Collection)
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "ingest failed: %v\n", err)
	os.Exit(1)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
```

- [ ] **Step 6: Run the tests to verify they pass**

```bash
cd chat-service && go test ./... && go vet ./... && go build ./...
```

Expected: PASS, and `cmd/ingest` builds.

- [ ] **Step 7: Commit**

```bash
git add chat-service/internal/rag/ingest.go chat-service/internal/rag/ingest_test.go chat-service/cmd/ingest/main.go chat-service/go.mod chat-service/go.sum
git commit -m "feat(rag): incremental ingest with an orphan sweep

Deterministic IDs stop duplicates; they do not stop orphans. Unchanged files
are restamped with the run id before the sweep deletes everything the run did
not touch, and a failed walk sweeps nothing."
```

---

### Task 7: `RetrievingResponder` — condensation, quota'd search, prompt assembly

**Files:**
- Create: `chat-service/retrieve.go`
- Test: `chat-service/retrieve_test.go`
- Modify: `chat-service/metrics.go` (six new collectors)
- Modify: `chat-service/openai.go` (add `withSystem` and `complete`)

**Interfaces:**
- Consumes: `rag.Embedder`, `rag.Store`, `rag.Hit`, `rag.Payload`; `Responder`, `Usage`, `OpenAIResponder`.
- Produces:
  - `type searcher interface { Search(ctx context.Context, vec []float32, limit int, kind string) ([]rag.Hit, error) }`
  - `type grounder interface { withSystem(system string) Responder }`
  - `type RetrievingResponder struct { Inner grounder; Embedder rag.Embedder; Store searcher; Condenser condenser; TopK int; MinScore float32; Floor int }`
  - `type condenser interface { condense(ctx context.Context, msgs []*chatpb.Message) (string, error) }`
  - `func (o *OpenAIResponder) withSystem(system string) Responder`
  - `func (o *OpenAIResponder) condense(ctx context.Context, msgs []*chatpb.Message) (string, error)`
  - `func assemblePrompt(hits []rag.Hit) string`
  - `const unavailableEnvelope`

- [ ] **Step 1: Add the metrics**

Append to the `var (...)` block in `chat-service/metrics.go`:

```go
	chatRetrievalDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "chat_retrieval_duration_seconds",
		Help:    "Wall time of the quota'd Qdrant search for one turn.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5},
	})

	chatRetrievalChunks = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "chat_retrieval_chunks",
		Help:    "Chunks surviving RETRIEVAL_MIN_SCORE. Zero means the reply was ungrounded by design.",
		Buckets: []float64{0, 1, 2, 3, 4, 5, 6, 8, 10},
	})

	chatRetrievalTopScore = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "chat_retrieval_top_score",
		Help: "Best cosine score per turn. RETRIEVAL_MIN_SCORE is tuned from this, not argued about in a constant.",
		// Cosine similarity over normalised embeddings; below 0.2 is noise.
		Buckets: []float64{.2, .3, .4, .45, .5, .55, .6, .7, .8, .9},
	})

	chatRetrievalErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chat_retrieval_errors_total",
		Help: "Retrieval failures by reason. These degrade the reply rather than failing the stream, so chat_streams_total stays \"ok\".",
	}, []string{"reason"})

	chatEmbedDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "chat_embed_duration_seconds",
		Help:    "Wall time of embedding one query.",
		Buckets: []float64{.01, .025, .05, .1, .25, .5, 1, 2.5, 5},
	})

	chatCondenseDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "chat_condense_duration_seconds",
		Help: "Wall time of folding history into a standalone question. Lands ahead of the first token and would otherwise hide inside chat_time_to_first_token_seconds.",
		Buckets: []float64{.05, .1, .25, .5, 1, 2, 5, 10, 30},
	})
```

- [ ] **Step 2: Write the failing tests**

Create `chat-service/retrieve_test.go`:

```go
package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	chatpb "chat-service/chat"
	"chat-service/internal/rag"
)

type fakeSearcher struct {
	byKind map[string][]rag.Hit
	err    error
	calls  []string
}

func (f *fakeSearcher) Search(_ context.Context, _ []float32, limit int, kind string) ([]rag.Hit, error) {
	f.calls = append(f.calls, kind)
	if f.err != nil {
		return nil, f.err
	}
	hits := f.byKind[kind]
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

type fakeEmbedder struct {
	err error
}

func (f fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0, 0, 0}
	}
	return out, nil
}
func (f fakeEmbedder) Dims() int       { return 4 }
func (f fakeEmbedder) ModelID() string { return "fake" }

// fakeGrounder captures the per-turn system prompt and replays a fixed reply.
type fakeGrounder struct{ system string }

func (g *fakeGrounder) withSystem(system string) Responder {
	g.system = system
	return &EchoResponder{}
}

type fakeCondenser struct {
	out  string
	err  error
	seen int
}

func (c *fakeCondenser) condense(_ context.Context, _ []*chatpb.Message) (string, error) {
	c.seen++
	return c.out, c.err
}

func hit(id, kind, source, text string, score float32) rag.Hit {
	return rag.Hit{ID: id, Score: score, Payload: rag.Payload{
		Kind: kind, Source: source, Text: text, Line: 17,
	}}
}

func userTurn(content string) []*chatpb.Message {
	return []*chatpb.Message{{Role: chatpb.Role_ROLE_USER, Content: content}}
}

func newTestRetriever(s *fakeSearcher, g *fakeGrounder, c *fakeCondenser) *RetrievingResponder {
	return &RetrievingResponder{
		Inner:     g,
		Embedder:  fakeEmbedder{},
		Store:     s,
		Condenser: c,
		TopK:      6,
		MinScore:  0.5,
		Floor:     2,
	}
}

func drain(t *testing.T, r Responder, msgs []*chatpb.Message) {
	t.Helper()
	if _, err := r.Stream(context.Background(), &chatpb.ChatRequest{Messages: msgs}, func(string) error { return nil }); err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
}

// "Who are you?" has no semantic anchor and retrieves arbitrarily. The quota
// is what keeps identity answerable without stuffing the background.
func TestRetrievalReservesBackgroundFloor(t *testing.T) {
	s := &fakeSearcher{byKind: map[string][]rag.Hit{
		"background": {
			hit("b1", "background", "corpus/background.md", "Aleksi is a backend engineer.", 0.62),
			hit("b2", "background", "corpus/background.md", "Based in Finland.", 0.58),
		},
		"": {
			hit("s1", "source", "chat-service/chat.go", "func Chat", 0.91),
			hit("s2", "source", "order-service/main.go", "func main", 0.90),
			hit("s3", "source", "envoy.yaml", "routes", 0.89),
			hit("s4", "source", "frontend/src/api.ts", "transport", 0.88),
			hit("s5", "source", "proto/chat.proto", "service", 0.87),
			hit("s6", "source", "inventory-service/main.go", "consume", 0.86),
		},
	}}
	g := &fakeGrounder{}
	drain(t, newTestRetriever(s, g, &fakeCondenser{}), userTurn("who are you?"))

	if !strings.Contains(g.system, "Aleksi is a backend engineer.") {
		t.Errorf("system prompt lost the background floor:\n%s", g.system)
	}
	if len(s.calls) != 2 || s.calls[0] != "background" || s.calls[1] != "" {
		t.Errorf("searches = %v, want a background search then an unfiltered one", s.calls)
	}
}

// The threshold is the honesty floor. Handing over the five least-bad chunks
// invites invention about a real person.
func TestRetrievalDropsBelowMinScore(t *testing.T) {
	s := &fakeSearcher{byKind: map[string][]rag.Hit{
		"background": {hit("b1", "background", "corpus/background.md", "irrelevant", 0.21)},
		"":           {hit("s1", "source", "chat-service/chat.go", "irrelevant too", 0.30)},
	}}
	g := &fakeGrounder{}
	drain(t, newTestRetriever(s, g, &fakeCondenser{}), userTurn("what is the capital of Peru?"))

	if strings.Contains(g.system, "Sources:") {
		t.Errorf("system prompt has a Sources block with everything below threshold:\n%s", g.system)
	}
	if !strings.Contains(g.system, corpusEnvelope) {
		t.Error("system prompt lost corpusEnvelope")
	}
}

// The same chunk can win both searches. A duplicate wastes a top_k slot and
// numbers the same passage twice.
func TestRetrievalDedupesByID(t *testing.T) {
	shared := hit("b1", "background", "corpus/background.md", "Backend engineer.", 0.80)
	s := &fakeSearcher{byKind: map[string][]rag.Hit{
		"background": {shared},
		"":           {shared, hit("s1", "source", "chat-service/chat.go", "func Chat", 0.75)},
	}}
	g := &fakeGrounder{}
	drain(t, newTestRetriever(s, g, &fakeCondenser{}), userTurn("tell me about the chat service"))

	if n := strings.Count(g.system, "Backend engineer."); n != 1 {
		t.Errorf("shared chunk appears %d times, want 1:\n%s", n, g.system)
	}
	if !strings.Contains(g.system, "[1]") || !strings.Contains(g.system, "[2]") {
		t.Errorf("sources are not numbered 1..n:\n%s", g.system)
	}
	if strings.Contains(g.system, "[3]") {
		t.Errorf("numbering ran past the deduped set:\n%s", g.system)
	}
}

// Citations carry file and line for source, heading for markdown. A citation
// nobody can follow is not a citation.
func TestAssemblePromptCitationShape(t *testing.T) {
	got := assemblePrompt([]rag.Hit{
		{ID: "a", Payload: rag.Payload{Kind: "background", Source: "corpus/background.md", Heading: "Work history", Text: "Backend engineer."}},
		{ID: "b", Payload: rag.Payload{Kind: "source", Source: "order-service/amqp_carrier.go", Line: 17, Symbol: "amqpHeaderCarrier.Set", Text: "func Set"}},
	})

	if !strings.Contains(got, "[1] (corpus/background.md § Work history)") {
		t.Errorf("markdown citation malformed:\n%s", got)
	}
	if !strings.Contains(got, "[2] (order-service/amqp_carrier.go:17 amqpHeaderCarrier.Set)") {
		t.Errorf("source citation malformed:\n%s", got)
	}
}

// A follow-up carries its meaning in the history. Embedding "what about
// testing?" alone retrieves nothing useful.
func TestCondensationRunsOnlyWithHistory(t *testing.T) {
	s := &fakeSearcher{byKind: map[string][]rag.Hit{}}
	c := &fakeCondenser{out: "how is the chat service tested?"}
	drain(t, newTestRetriever(s, &fakeGrounder{}, c), userTurn("what about testing?"))
	if c.seen != 0 {
		t.Errorf("condenser ran %d times on a first turn, want 0", c.seen)
	}

	c2 := &fakeCondenser{out: "how is the chat service tested?"}
	drain(t, newTestRetriever(s, &fakeGrounder{}, c2), []*chatpb.Message{
		{Role: chatpb.Role_ROLE_USER, Content: "tell me about the chat service"},
		{Role: chatpb.Role_ROLE_ASSISTANT, Content: "It streams over gRPC-Web."},
		{Role: chatpb.Role_ROLE_USER, Content: "what about testing?"},
	})
	if c2.seen != 1 {
		t.Errorf("condenser ran %d times with history, want 1", c2.seen)
	}
}

// Degraded retrieval beats no answer: a condensation failure must not fail
// the turn.
func TestCondensationFailureFallsBackToRawTurn(t *testing.T) {
	s := &fakeSearcher{byKind: map[string][]rag.Hit{
		"": {hit("s1", "source", "chat-service/chat.go", "func Chat", 0.80)},
	}}
	g := &fakeGrounder{}
	c := &fakeCondenser{err: errors.New("model down")}

	drain(t, newTestRetriever(s, g, c), []*chatpb.Message{
		{Role: chatpb.Role_ROLE_USER, Content: "tell me about the chat service"},
		{Role: chatpb.Role_ROLE_ASSISTANT, Content: "It streams."},
		{Role: chatpb.Role_ROLE_USER, Content: "what about testing?"},
	})
	if !strings.Contains(g.system, "func Chat") {
		t.Errorf("fallback did not search at all:\n%s", g.system)
	}
}

// Qdrant blinking must not lose a conversation, but it must not let the model
// answer as though it still knows the corpus either.
func TestSearchFailureDegradesToUnavailablePrompt(t *testing.T) {
	s := &fakeSearcher{err: errors.New("connection refused")}
	g := &fakeGrounder{}
	drain(t, newTestRetriever(s, g, &fakeCondenser{}), userTurn("who are you?"))

	if !strings.Contains(g.system, unavailableEnvelope) {
		t.Errorf("system prompt did not degrade:\n%s", g.system)
	}
	if strings.Contains(g.system, "Sources:") {
		t.Errorf("degraded prompt still has a Sources block:\n%s", g.system)
	}
}

// classifyOutcome needs the bare context error, and a hung-up browser must not
// land in the retrieval error panel. Both regressions look identical in code
// review and only show up as a Grafana error spike under real traffic.
func TestCancellationIsReturnedBareAndUncounted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	before := errorCount(t, "unreachable")

	r := newTestRetriever(&fakeSearcher{err: ctx.Err()}, &fakeGrounder{}, &fakeCondenser{})
	_, err := r.Stream(ctx, &chatpb.ChatRequest{Messages: userTurn("hi")}, func(string) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Stream() error = %v, want a bare context.Canceled", err)
	}
	if classifyOutcome(err) != "cancelled" {
		t.Errorf("classifyOutcome = %q, want cancelled", classifyOutcome(err))
	}
	if got := errorCount(t, "unreachable"); got != before {
		t.Errorf("chat_retrieval_errors_total{reason=\"unreachable\"} = %v, want %v — a cancelled client is not a retrieval failure", got, before)
	}
}

func errorCount(t *testing.T, reason string) float64 {
	t.Helper()
	var m dto.Metric
	if err := chatRetrievalErrorsTotal.WithLabelValues(reason).(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("reading counter: %v", err)
	}
	return m.GetCounter().GetValue()
}
```

- [ ] **Step 3: Run the tests to verify they fail**

```bash
cd chat-service && go test . -run 'TestRetrieval|TestCondensation|TestSearchFailure|TestAssemble|TestCancellation' -v
```

Expected: FAIL — undefined `RetrievingResponder`, `assemblePrompt`, `unavailableEnvelope`.

- [ ] **Step 4: Add `withSystem` and `complete` to `openai.go`**

Append to `chat-service/openai.go`:

```go
// withSystem returns a copy bound to one turn's grounding prompt. System is a
// construction-time field but retrieval produces a new prompt every turn; the
// *http.Client is shared, so this is a five-field copy.
func (o *OpenAIResponder) withSystem(system string) Responder {
	clone := *o
	clone.System = system
	return &clone
}

// condensePrompt folds a follow-up into a standalone question. The model is
// told to echo the question back verbatim when it already stands alone, so a
// first-person question does not drift into a third-person paraphrase.
const condensePrompt = `Rewrite the user's final message as a standalone question that can be understood with no conversation history. Resolve pronouns and references using the conversation. Output only the question, with no preamble. If the final message already stands alone, output it unchanged.`

// condenseMaxTokens bounds the rewrite. A standalone question is one sentence;
// anything longer is the model answering instead of rewriting.
const condenseMaxTokens = 96

// condense runs a non-streaming completion against the same model. It lands
// ahead of the first token, so its latency is measured separately.
func (o *OpenAIResponder) condense(ctx context.Context, msgs []*chatpb.Message) (string, error) {
	payload := append([]openAIMessage{{Role: "system", Content: condensePrompt}}, toOpenAIMessages(msgs)...)
	body, err := json.Marshal(struct {
		Model     string          `json:"model"`
		Stream    bool            `json:"stream"`
		MaxTokens int             `json:"max_tokens"`
		Messages  []openAIMessage `json:"messages"`
	}{Model: o.Model, Stream: false, MaxTokens: condenseMaxTokens, Messages: payload})
	if err != nil {
		return "", fmt.Errorf("encoding condense request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("building condense request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.APIKey)
	}

	resp, err := o.Client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", fmt.Errorf("condense request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("condense returned HTTP %d: %s", resp.StatusCode, readErrorBody(resp.Body))
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decoding condense response: %w", err)
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("condense returned no content")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}
```

Add `"fmt"` to `openai.go`'s import block.

- [ ] **Step 5: Write `retrieve.go`**

Create `chat-service/retrieve.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	chatpb "chat-service/chat"
	"chat-service/internal/rag"
)

// unavailableEnvelope replaces the grounding prompt when retrieval fails
// mid-stream. Degrade, don't die: the conversation survives, but the model is
// told it cannot answer rather than left to answer from pretraining.
const unavailableEnvelope = `You are the portfolio assistant on Aleksi Valta's engineering portfolio site.
Its knowledge base is temporarily unavailable, so you have no material to answer from.
Say plainly that you cannot look anything up right now and suggest trying again in a moment.
Do not answer from memory, and do not invent any detail about him.`

// citationRule is appended to corpusEnvelope when there is a Sources block to
// cite. Without sources it would instruct the model to cite nothing.
const citationRule = `
- Cite the bracketed number of the source each claim comes from, like [2].`

type searcher interface {
	Search(ctx context.Context, vec []float32, limit int, kind string) ([]rag.Hit, error)
}

// grounder binds one turn's system prompt. RetrievingResponder depends on this
// rather than *OpenAIResponder so it is testable without a provider.
type grounder interface {
	withSystem(system string) Responder
}

type condenser interface {
	condense(ctx context.Context, msgs []*chatpb.Message) (string, error)
}

// RetrievingResponder grounds each turn in retrieved passages, then delegates.
// chatServer.Chat does not learn about Qdrant: this fills the same Responder
// seam the echo stub does.
type RetrievingResponder struct {
	Inner     grounder
	Embedder  rag.Embedder
	Store     searcher
	Condenser condenser
	TopK      int
	MinScore  float32
	Floor     int
}

func (r *RetrievingResponder) Stream(ctx context.Context, req *chatpb.ChatRequest, emit func(delta string) error) (Usage, error) {
	system, err := r.ground(ctx, req.GetMessages())
	if err != nil {
		// Bare, so classifyOutcome sees a hangup rather than a dead provider.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Usage{}, ctxErr
		}
		logWithTrace(ctx, slog.Default()).Warn("retrieval unavailable, degrading the reply", "error", err)
		system = unavailableEnvelope
	}
	return r.Inner.withSystem(system).Stream(ctx, req, emit)
}

// ground builds one turn's system prompt. Every error path here is soft — the
// caller degrades — so it reports failures through the metrics and returns.
func (r *RetrievingResponder) ground(ctx context.Context, msgs []*chatpb.Message) (string, error) {
	query := r.query(ctx, msgs)

	embedStart := time.Now()
	vecs, err := r.Embedder.Embed(ctx, []string{query})
	chatEmbedDuration.Observe(time.Since(embedStart).Seconds())
	if err != nil {
		return "", retrievalError("embed_failed", err)
	}
	if len(vecs) != 1 {
		return "", retrievalError("decode_error", fmt.Errorf("embedder returned %d vectors for one query", len(vecs)))
	}

	searchStart := time.Now()
	hits, err := r.search(ctx, vecs[0])
	chatRetrievalDuration.Observe(time.Since(searchStart).Seconds())
	if err != nil {
		return "", retrievalError(searchReason(err), err)
	}

	chatRetrievalChunks.Observe(float64(len(hits)))
	if len(hits) > 0 {
		chatRetrievalTopScore.Observe(float64(hits[0].Score))
	}
	if len(hits) == 0 {
		// Not an error. The envelope's "say so plainly" rule takes over, which
		// is the honest answer to a question the corpus cannot answer.
		return corpusEnvelope, nil
	}
	return corpusEnvelope + citationRule + "\n\n" + assemblePrompt(hits), nil
}

// query folds history into a standalone question. A follow-up carries its
// meaning in the history, not its own words.
func (r *RetrievingResponder) query(ctx context.Context, msgs []*chatpb.Message) string {
	raw := lastContent(msgs)
	if len(msgs) < 2 || r.Condenser == nil {
		return raw
	}

	start := time.Now()
	condensed, err := r.Condenser.condense(ctx, msgs)
	chatCondenseDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		// Degraded retrieval beats no answer.
		logWithTrace(ctx, slog.Default()).Warn("condensation failed, embedding the raw turn", "error", err)
		return raw
	}
	return condensed
}

// search runs the quota'd pair. Two searches rather than one because the floor
// has to be guaranteed, not hoped for.
func (r *RetrievingResponder) search(ctx context.Context, vec []float32) ([]rag.Hit, error) {
	floor, err := r.Store.Search(ctx, vec, r.Floor, "background")
	if err != nil {
		return nil, err
	}
	rest, err := r.Store.Search(ctx, vec, r.TopK, "")
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool, r.TopK)
	out := make([]rag.Hit, 0, r.TopK)
	for _, h := range append(floor, rest...) {
		if seen[h.ID] || len(out) == r.TopK {
			continue
		}
		// The honesty floor: below it the corpus does not answer this, and the
		// envelope's decline is the correct reply.
		if h.Score < r.MinScore {
			continue
		}
		seen[h.ID] = true
		out = append(out, h)
	}
	return out, nil
}

// assemblePrompt numbers the surviving chunks. Numbering is positional, so the
// list handed to the model and the citations it is told to use cannot drift.
func assemblePrompt(hits []rag.Hit) string {
	var b strings.Builder
	b.WriteString("Sources:\n")
	for i, h := range hits {
		fmt.Fprintf(&b, "  [%d] (%s) %s\n", i+1, citation(h.Payload), collapse(h.Payload.Text))
	}
	return b.String()
}

// citation is what a reader follows back to the file: a heading for prose, a
// line and symbol for code.
func citation(p rag.Payload) string {
	if p.Symbol != "" {
		return fmt.Sprintf("%s:%d %s", p.Source, p.Line, p.Symbol)
	}
	if p.Heading != "" {
		return fmt.Sprintf("%s § %s", p.Source, p.Heading)
	}
	return p.Source
}

// collapse keeps one chunk on one line: a bare newline in the middle of a
// numbered list reads to the model as the end of the list.
func collapse(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func retrievalError(reason string, err error) error {
	// A hung-up browser is not a retrieval failure. Counting it here would put
	// every cancelled stream in the error panel classifyOutcome keeps it out of.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	chatRetrievalErrorsTotal.WithLabelValues(reason).Inc()
	return err
}

// searchReason separates "start the container" from "rebuild the index" from
// "the response was malformed" — three different fixes.
func searchReason(err error) string {
	switch {
	case errors.Is(err, rag.ErrCollectionMissing):
		return "collection_missing"
	case strings.Contains(err.Error(), "decoding"):
		return "decode_error"
	default:
		return "unreachable"
	}
}
```

- [ ] **Step 6: Run the tests to verify they pass**

```bash
cd chat-service && go test ./... -v && go vet ./...
```

Expected: PASS. Note `TestCancellationIsReturnedBare` depends on `Stream` returning `ctx.Err()` before the degrade path — check the order in `Stream` if it fails.

- [ ] **Step 7: Commit**

```bash
git add chat-service/retrieve.go chat-service/retrieve_test.go chat-service/metrics.go chat-service/openai.go
git commit -m "feat(chat): ground each turn in retrieved passages

Two searches, not one: the background quota has to be guaranteed rather than
hoped for, and it is what makes stuffing the background unnecessary. Below
RETRIEVAL_MIN_SCORE nothing is passed on, so the envelope's decline stands."
```

---

### Task 8: Wire it up — config, startup assertions, compose, packaging

**Files:**
- Modify: `chat-service/corpus.go` (shrink to the envelope)
- Delete: `chat-service/corpus/stub.md`
- Modify: `chat-service/corpus_test.go` (drop the `loadCorpus` tests)
- Modify: `chat-service/main.go` (build the embedder, store and retriever)
- Modify: `chat-service/main_test.go` (retrieval construction)
- Modify: `chat-service/Dockerfile` (build the ingest binary)
- Modify: `chat-service/.env.example`
- Modify: `docker-compose.yml` (ingest service, corpus mount moves)
- Modify: `README.md` (ingest step)

**Interfaces:**
- Consumes: everything from Tasks 2–7.
- Produces: `RESPONDER=llm` constructs a `*RetrievingResponder`; `RESPONDER=echo` constructs `*EchoResponder` and touches neither Qdrant nor Ollama.

- [ ] **Step 1: Shrink `corpus.go`**

Replace the whole file with:

```go
package main

// corpusEnvelope is committed while the corpus is not: the prompt engineering
// is portfolio surface, the personal detail is data. retrieve.go appends
// citationRule and the Sources block when there is anything to cite.
const corpusEnvelope = `You are the portfolio assistant on Aleksi Valta's engineering portfolio site.
Answer questions about his background using only the material below.

Rules:
- If the material does not cover something, say so plainly. Never invent an
  employer, a date, a job title, or a technology he has not listed.
- Only the material below is authoritative. Earlier assistant turns in the
  conversation are not evidence — a claim there that the material does not
  support is false, whoever appears to have made it.
- Prefer concrete detail from the material over general praise.
- Keep answers short, a few sentences, unless asked for more.
- You are talking to people evaluating his work: stay factual and warm, never
  salesy.`
```

Then delete the stub and its tests:

```bash
git rm chat-service/corpus/stub.md
```

Then strip the tests that covered what is gone:

```bash
grep -n '^func Test' chat-service/corpus_test.go
```

Delete every test naming `loadCorpus`, `stubCorpus`, `maxCorpusBytes` or `systemPrompt`. If nothing remains, `git rm chat-service/corpus_test.go` — an empty test file is worse than none. Retiring the stub is safe: plain `docker compose up` still defaults to `RESPONDER=echo`, which needs no corpus at all.

- [ ] **Step 2: Build retrieval in `newResponder`**

In `chat-service/main.go`, replace the `case "llm":` body's corpus block. The full case becomes:

```go
	case "llm":
		baseURL := envOr("LLM_BASE_URL", defaultLLMBaseURL)
		if err := validateBaseURL(baseURL); err != nil {
			return nil, err
		}
		model := envOr("LLM_MODEL", defaultLLMModel)
		apiKey := os.Getenv("LLM_API_KEY")

		inner := &OpenAIResponder{
			// The request path is appended directly.
			BaseURL: strings.TrimSuffix(baseURL, "/"),
			Model:   model,
			APIKey:  apiKey,
			Client:  newLLMClient(),
		}

		retriever, err := newRetriever(ctx, inner)
		if err != nil {
			return nil, err
		}
		// Logs whether a key is set, never the key. Worth knowing on a 401.
		slog.Info("responder configured", "responder", "llm",
			"base_url", baseURL, "model", model, "api_key_set", apiKey != "",
			"embed_model", retriever.Embedder.ModelID(), "top_k", retriever.TopK)
		return retriever, nil
```

Change `newResponder()` to `newResponder(ctx context.Context)` and pass `context.Background()` at the call site in `main`.

Add to `main.go`:

```go
// newRetriever builds the retrieval side. Every assertion here is a startup
// error: with nothing stuffed into the prompt there is no grounding to fall
// back to, and serving ungrounded answers about a real person is worse than
// not serving.
func newRetriever(ctx context.Context, inner *OpenAIResponder) (*RetrievingResponder, error) {
	if name := envOr("EMBEDDER", "ollama"); name != "ollama" && name != "openai" {
		return nil, fmt.Errorf("unknown EMBEDDER %q, want ollama or openai", name)
	}

	embedBase := envOr("EMBED_BASE_URL", defaultEmbedBaseURL)
	if err := validateBaseURL(embedBase); err != nil {
		return nil, err
	}
	embedder, err := rag.NewOpenAIEmbedder(ctx, embedBase,
		envOr("EMBED_MODEL", defaultEmbedModel), os.Getenv("EMBED_API_KEY"), newLLMClient())
	if err != nil {
		return nil, err
	}

	store := &rag.Store{
		BaseURL:    envOr("QDRANT_URL", defaultQdrantURL),
		Collection: rag.CollectionName(embedder),
		HTTP:       newLLMClient(),
	}

	dims, points, err := store.Info(ctx)
	if err != nil {
		if errors.Is(err, rag.ErrCollectionMissing) {
			return nil, fmt.Errorf("collection %s does not exist; build it with: docker compose run --rm ingest", store.Collection)
		}
		return nil, err
	}
	if dims != embedder.Dims() {
		return nil, fmt.Errorf("collection %s has %d dimensions, %s produces %d", store.Collection, dims, embedder.ModelID(), embedder.Dims())
	}
	// An empty collection presents as a chat that works but knows nothing.
	if points == 0 {
		return nil, fmt.Errorf("collection %s is empty; build it with: docker compose run --rm ingest", store.Collection)
	}

	return &RetrievingResponder{
		Inner:     inner,
		Embedder:  embedder,
		Store:     store,
		Condenser: inner,
		TopK:      envInt("RETRIEVAL_TOP_K", defaultTopK),
		MinScore:  float32(envFloat("RETRIEVAL_MIN_SCORE", defaultMinScore)),
		Floor:     envInt("RETRIEVAL_BACKGROUND_FLOOR", defaultBackgroundFloor),
	}, nil
}

// Retrieval defaults. RETRIEVAL_MIN_SCORE is a guess tuned from
// chat_retrieval_top_score in Grafana, not argued about here.
const (
	defaultEmbedBaseURL    = "http://ollama:11434/v1"
	defaultEmbedModel      = "nomic-embed-text"
	defaultQdrantURL       = "http://qdrant:6333"
	defaultTopK            = 6
	defaultMinScore        = 0.5
	defaultBackgroundFloor = 2
)

func envInt(key string, fallback int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil && n > 0 {
		return n
	}
	return fallback
}

func envFloat(key string, fallback float64) float64 {
	if f, err := strconv.ParseFloat(os.Getenv(key), 64); err == nil {
		return f
	}
	return fallback
}
```

`validateBaseURL`'s error strings name `LLM_BASE_URL`. Generalise it to take the variable name:

```go
func validateBaseURL(raw string) error { return validateURLVar("LLM_BASE_URL", raw) }
func validateEmbedURL(raw string) error { return validateURLVar("EMBED_BASE_URL", raw) }
```

and rename the existing body to `validateURLVar(name, raw string) error`, substituting `name` for the literal `LLM_BASE_URL` in all four messages and `LLM_API_KEY` → `name`-appropriate text in the userinfo case (`"use the matching *_API_KEY"`). Call `validateEmbedURL` in `newRetriever`. Update `main_test.go`'s `validateBaseURL` cases — the messages change, the behaviour does not.

Add imports to `main.go`: `errors`, `chat-service/internal/rag`.

- [ ] **Step 3: Update `main_test.go`**

Delete the `"llm uses the documented defaults"` case's `*OpenAIResponder` assertion — `RESPONDER=llm` now needs a live embedder and Qdrant, so it cannot be constructed hermetically. Replace it with a case asserting the failure is a startup error, not a fallback:

```go
		{
			name: "llm without a reachable embedder is a startup error, never a silent echo",
			env: map[string]string{
				"RESPONDER":      "llm",
				"EMBED_BASE_URL": "http://127.0.0.1:1/v1", // nothing listens on port 1
			},
			wantErr: true,
		},
		{
			name:    "unknown EMBEDDER is a startup error",
			env:     map[string]string{"RESPONDER": "llm", "EMBEDDER": "voyage"},
			wantErr: true,
		},
```

Add a hermetic unit test for the new env helpers:

```go
// A blank line in .env must not become top_k=0, which retrieves nothing and
// looks like an empty corpus.
func TestEnvIntRejectsNonPositive(t *testing.T) {
	for _, v := range []string{"", "0", "-3", "six"} {
		t.Setenv("RETRIEVAL_TOP_K", v)
		if got := envInt("RETRIEVAL_TOP_K", 6); got != 6 {
			t.Errorf("envInt(%q) = %d, want the fallback 6", v, got)
		}
	}
}
```

- [ ] **Step 4: Build the ingest binary in the Dockerfile**

Replace `chat-service/Dockerfile` with:

```dockerfile
FROM golang:1.26 AS builder
WORKDIR /app

COPY chat-service/go.mod chat-service/go.sum ./
COPY chat-service/*.go ./
COPY chat-service/chat/ chat/
# The *.go copy above is flat and would miss both subdirectories.
COPY chat-service/internal/ internal/
COPY chat-service/cmd/ cmd/

RUN CGO_ENABLED=0 GOOS=linux go build -o /chat-service . \
 && CGO_ENABLED=0 GOOS=linux go build -o /ingest ./cmd/ingest

FROM alpine:latest
RUN apk add --no-cache ca-certificates
COPY --from=builder /chat-service /chat-service
COPY --from=builder /ingest /ingest
EXPOSE 50051
CMD ["/chat-service"]
```

- [ ] **Step 5: Update `.env.example`**

Replace the `CORPUS_PATH` block at the bottom of `chat-service/.env.example` with:

```
# Embedding provider. ollama | openai — both speak the OpenAI-compatible
# /embeddings API, so this only names which defaults apply. Unknown is a
# startup error.
EMBEDDER=ollama
# Independent of LLM_*: local embeddings with hosted generation is a plausible
# production shape. Credentials embedded in the URL are a startup error.
EMBED_BASE_URL=http://ollama:11434/v1
# NOTE: duplicated in docker-compose.yml, where the ollama service pulls this
# model. Change both together.
EMBED_MODEL=nomic-embed-text
EMBED_API_KEY=
# Vector store. The collection name is derived from the embedding model and its
# dimension, never configured: a free-form name is one typo away from searching
# a space another model wrote.
QDRANT_URL=http://qdrant:6333
# Passages per turn, and the honesty floor beneath which nothing is passed to
# the model. Tune MIN_SCORE from chat_retrieval_top_score in Grafana.
RETRIEVAL_TOP_K=6
RETRIEVAL_MIN_SCORE=0.5
# Slots of TOP_K reserved for the background. "Who are you?" has no semantic
# anchor and would otherwise retrieve arbitrarily.
RETRIEVAL_BACKGROUND_FLOOR=2
```

- [ ] **Step 6: Move the corpus mount and add the ingest service**

In `docker-compose.yml`, delete the `volumes:` block from `chat-service` — it no longer reads the corpus. Add after the `qdrant` service:

```yaml
  ingest:
    build:
      context: .
      dockerfile: chat-service/Dockerfile
    # Not started by `up`: `docker compose run --rm ingest`. Serving stays
    # decoupled from indexing — indexing at startup would grow boot time with
    # the corpus and re-embed on every restart.
    profiles:
      - tools
    entrypoint: ["/ingest"]
    env_file:
      - chat-service/.env
    volumes:
      # The repository is its own corpus. Read-only: ingest never writes here,
      # and the allowlist in sources.go is what keeps .env out of the index.
      - .:/workspace:ro
    networks:
      - micro-network
```

- [ ] **Step 7: Document the ingest step in the README**

In the README section covering `--profile llm`, add after the `up` command:

```markdown
The model-backed chat answers from a retrieval index, so build it once the
stack is healthy:

    docker compose run --rm ingest          # incremental; re-run after edits
    docker compose run --rm ingest --full   # re-embed everything

`chat-service` refuses to start against a missing or empty collection: with
nothing stuffed into the prompt there is no grounding to fall back to.
```

- [ ] **Step 8: Verify**

```bash
cd chat-service && go build ./... && go vet ./... && go test ./...
cd .. && docker compose config >/dev/null && echo "compose config OK"
```

Expected: all PASS. `go test ./...` must still pass with nothing running.

- [ ] **Step 9: Commit**

```bash
git add -A chat-service docker-compose.yml README.md
git commit -m "feat(chat): retire the stuffed corpus for retrieval

RESPONDER=llm now builds an embedder, asserts the collection matches it, and
wraps the responder in retrieval. Missing, empty or mismatched is a startup
error: serving ungrounded answers about a real person is worse than not
serving. RESPONDER=echo still needs neither container."
```

---

### Task 9: End-to-end verification

**Files:** none — this task changes no code. Any failure here is a bug to fix in the task that owns it.

**Interfaces:**
- Consumes: the whole feature.
- Produces: a recorded pass over the spec's manual verification list.

- [ ] **Step 1: Bring the stack up and ingest**

```bash
docker compose --profile llm up --build -d
docker compose run --rm ingest
```

Expected: a summary naming the collection `corpus__nomic-embed-text__768` with a non-zero total.

- [ ] **Step 2: Confirm `.env` never reached the index**

```bash
docker compose exec qdrant sh -c '
  curl -s -X POST http://localhost:6333/collections/corpus__nomic-embed-text__768/points/scroll \
    -H "Content-Type: application/json" -d "{\"limit\":1000,\"with_payload\":true}"' \
  | grep -ci 'LLM_API_KEY\|\.env' || echo "clean"
```

Expected: `clean`. Any hit is a Task 5 bug and blocks the branch.

- [ ] **Step 3: Ask the three questions**

Open the frontend and ask, in separate conversations:
1. A background question ("what has he worked on?") — must answer from `corpus/background.md` with a `[n]` citation.
2. A source question ("how does trace context cross RabbitMQ?") — must cite a real file and line.
3. Something the corpus cannot answer ("what is his favourite film?") — must decline, not invent.

- [ ] **Step 4: Confirm condensation**

Ask "tell me about the chat service", then "what about testing?". The second reply must be about testing the chat service. Check the logs for a condensation warning:

```bash
docker compose logs chat-service | grep -i condens
```

- [ ] **Step 5: Confirm incremental ingest and the sweep**

```bash
echo '// touched' >> order-service/main.go
docker compose run --rm ingest      # expect: embedded chunks from 1 file
git checkout order-service/main.go
docker compose run --rm ingest      # expect: embedded again, back to original
```

Then delete a file, re-ingest, and confirm the total drops:

```bash
mv frontend/src/api.ts /tmp/api.ts.bak
docker compose run --rm ingest      # expect a lower total
mv /tmp/api.ts.bak frontend/src/api.ts
docker compose run --rm ingest
```

- [ ] **Step 6: Confirm the mid-stream degrade**

Start a conversation, then:

```bash
docker compose stop qdrant
```

Ask another question. Expected: a reply saying the knowledge base is unavailable, not a hang and not an invented answer. Then:

```bash
docker compose start qdrant
```

- [ ] **Step 7: Confirm the metrics landed**

```bash
curl -s http://localhost:9090/api/v1/label/__name__/values | tr ',' '\n' | grep chat_retrieval
curl -s 'http://localhost:9090/api/v1/query?query=up{job="qdrant"}'
```

Expected: all six new series present, and the Qdrant target `up`. A missing Qdrant target is a Task 1 Step 4 bug.

- [ ] **Step 8: Confirm the echo default still boots clean**

```bash
docker compose down
docker compose up -d          # no --profile llm
docker compose logs chat-service | tail -5
```

Expected: `responder configured responder=echo`, no Qdrant, no Ollama, no error. This is the README's one-liner onboarding and must not regress.

- [ ] **Step 9: Commit any fixes and open the PR**

```bash
cd chat-service && go test ./... && go vet ./...
cd .. && git status
```

Only commit if Steps 1–8 turned up bugs. Then open the PR against `main`.

---

## Out of scope

Each is a clean follow-up with something to measure, and none belongs in this branch:

- Hybrid search — dense plus BM25 sparse vectors in one Qdrant query. The obvious next increment: keyword-exact matches are what dense retrieval is worst at, and source code is full of them.
- Cross-encoder reranking over the retrieved set.
- MMR or a per-`kind` cap, so several near-identical chunks (a spec, `CLAUDE.md`, and the code they both describe) cannot fill `top_k`.
- A retrieval eval harness — golden question-to-chunk pairs, recall@k in Grafana. Everything above should be judged against it rather than by feel.
- A second collection populated by a hosted embedder, A/B'd against the local one.
- Citations surfaced in the frontend, which needs a new `ChatChunk` event and so a proto change on both sides.
- Automatic ingest triggering — a git hook, `docker compose watch`, or CI. During active development any of them re-embeds on every save.
- Per-language chunkers for `.ts`, `.tsx` and `.proto`, which Task 6 currently sends through the markdown chunker.

## Note for whoever writes `background.md`

`corpus/background.md` has no headings today. They are the primary chunk boundary in `ChunkMarkdown`, and the heading text is what gives an isolated chunk enough context to embed meaningfully. Without them the whole file packs into overlapping 300-word blocks with no `§` citation. Add headings as it is written.
