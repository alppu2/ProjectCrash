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
			t.Errorf("decoding request: %v", err)
			return
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
func (f fakeEmbedder) Dims() int       { return f.dims }
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
