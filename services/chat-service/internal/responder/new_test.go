package responder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"chat-service/internal/guard"
	"chat-service/internal/rag"
)

const (
	testEmbedModel = "nomic-embed-text"
	testEmbedDims  = 3
	// rag.CollectionName for testEmbedModel at testEmbedDims.
	testCollection = "corpus__nomic-embed-text__3"
)

// fakeBackend serves both the embedder probe and Qdrant's collection info, so
// one URL fills EmbedBaseURL and QdrantURL. collection is what Qdrant reports
// for testCollection; nil means it does not exist.
type fakeBackend struct {
	collection *struct{ dims, points int }
	qdrantCode int // overrides the response when non-zero
	guardCode  int // /health status; zero means 200
}

func (f *fakeBackend) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			if f.guardCode != 0 {
				w.WriteHeader(f.guardCode)
			}
		case r.URL.Path == "/embeddings":
			vec := make([]float32, testEmbedDims)
			json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"embedding": vec, "index": 0}},
			})
		case f.qdrantCode != 0:
			w.WriteHeader(f.qdrantCode)
		case r.URL.Path == "/collections/"+testCollection && f.collection != nil:
			json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{
				"points_count": f.collection.points,
				"config": map[string]any{"params": map[string]any{
					"vectors": map[string]any{"size": f.collection.dims},
				}},
			}})
		default:
			// Also what a lookup under any other collection name gets.
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func llmConfig(url string) Config {
	return Config{
		Kind:           "llm",
		BaseURL:        "http://llm.invalid/v1/",
		Model:          "qwen3",
		EmbedBaseURL:   url,
		EmbedModel:     testEmbedModel,
		QdrantURL:      url,
		TopK:           5,
		MinScore:       0.4,
		Floor:          2,
		GuardURL:       url,
		GuardThreshold: 0.7,
	}
}

// An unknown RESPONDER falling back to echo would serve a demo that looks like
// a working model and is not one.
func TestNewRejectsUnknownKind(t *testing.T) {
	for _, kind := range []string{"", "openai", "LLM"} {
		r, err := New(context.Background(), Config{Kind: kind})
		if err == nil || r != nil {
			t.Errorf("New(Kind=%q) = %T, %v; want nil and an error", kind, r, err)
		}
	}
}

func TestNewEcho(t *testing.T) {
	r, err := New(context.Background(), Config{Kind: "echo", EchoDelay: 7 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	echo, ok := r.(*EchoResponder)
	if !ok || echo.Delay != 7*time.Millisecond {
		t.Fatalf("New(echo) = %#v, want *EchoResponder with the configured delay", r)
	}
}

// Each case is a state in which llm mode would answer questions about a real
// person with nothing retrieved to ground them. Starting is the bug.
func TestNewLLMRefusesUngroundedStart(t *testing.T) {
	tests := []struct {
		name    string
		backend fakeBackend
		wantErr string
	}{
		{"collection missing", fakeBackend{}, "docker compose run --rm ingest"},
		{"collection empty", fakeBackend{collection: &struct{ dims, points int }{testEmbedDims, 0}}, "is empty"},
		{"dimension mismatch", fakeBackend{collection: &struct{ dims, points int }{768, 40}}, "768 dimensions"},
		{"qdrant failing", fakeBackend{qdrantCode: http.StatusInternalServerError}, "HTTP 500"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := tt.backend.serve(t)
			r, err := New(context.Background(), llmConfig(srv.URL))
			if err == nil || r != nil {
				t.Fatalf("New = %T, %v; want nil and an error", r, err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestNewLLMRefusesUnreachableEmbedder(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	if r, err := New(context.Background(), llmConfig(srv.URL)); err == nil || r != nil {
		t.Fatalf("New = %T, %v; want nil and an error", r, err)
	}
}

// Guards the wiring main.go relies on: the derived collection, the retrieval
// knobs, and one model client answering, condensing and warming.
func TestNewLLMWiresRetriever(t *testing.T) {
	backend := fakeBackend{collection: &struct{ dims, points int }{testEmbedDims, 40}}
	srv := backend.serve(t)

	r, err := New(context.Background(), llmConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	rr, ok := r.(*RetrievingResponder)
	if !ok {
		t.Fatalf("New(llm) = %T, want *RetrievingResponder", r)
	}

	if rr.TopK != 5 || rr.MinScore != 0.4 || rr.Floor != 2 {
		t.Errorf("TopK, MinScore, Floor = %d, %v, %d; want 5, 0.4, 2", rr.TopK, rr.MinScore, rr.Floor)
	}
	store, ok := rr.Store.(*rag.Store)
	if !ok || store.Collection != testCollection {
		t.Errorf("Store = %#v, want a *rag.Store on %s", rr.Store, testCollection)
	}

	inner, ok := rr.Inner.(*OpenAIResponder)
	if !ok {
		t.Fatalf("Inner = %T, want *OpenAIResponder", rr.Inner)
	}
	if inner.BaseURL != "http://llm.invalid/v1" || inner.Model != "qwen3" {
		t.Errorf("Inner BaseURL, Model = %q, %q; want the trailing slash trimmed", inner.BaseURL, inner.Model)
	}
	if rr.Condenser != condenser(inner) || rr.Warm != warmer(inner) {
		t.Error("Condenser and Warm should be the same client as Inner")
	}
	g, ok := rr.Guard.(*guard.TEIGuard)
	if !ok || g.BaseURL != srv.URL || g.Threshold != 0.7 {
		t.Errorf("Guard = %#v, want a *guard.TEIGuard on %s at threshold 0.7", rr.Guard, srv.URL)
	}
}

// A guard that never loaded would otherwise fail open on every turn with
// nothing but a warning per request.
func TestNewLLMRefusesUnreadyGuard(t *testing.T) {
	backend := fakeBackend{collection: &struct{ dims, points int }{testEmbedDims, 40}, guardCode: http.StatusServiceUnavailable}
	srv := backend.serve(t)
	r, err := New(context.Background(), llmConfig(srv.URL))
	if err == nil || r != nil {
		t.Fatalf("New = %T, %v; want a refusal to start without the guard", r, err)
	}
	if !strings.Contains(err.Error(), "guard") {
		t.Errorf("error %q does not name the guard", err)
	}
}
