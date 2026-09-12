package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewResponder(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
		check   func(t *testing.T, r Responder)
	}{
		{
			name: "defaults to echo so an empty .env still boots",
			// Explicitly empty, not unset: envOr treats both as absent, and
			// this survives a shell that exports RESPONDER.
			env: map[string]string{"RESPONDER": ""},
			check: func(t *testing.T, r Responder) {
				if _, ok := r.(*EchoResponder); !ok {
					t.Errorf("responder = %T, want *EchoResponder", r)
				}
			},
		},
		{
			// With nothing stuffed into the prompt there is no grounding to
			// fall back to, so an unreachable embedder must not degrade to echo.
			name:    "llm without a reachable embedder is a startup error, never a silent echo",
			env:     map[string]string{"RESPONDER": "llm"},
			wantErr: true,
		},
		{
			name:    "an unknown EMBEDDER is a startup error",
			env:     map[string]string{"RESPONDER": "llm", "EMBEDDER": "voyage"},
			wantErr: true,
		},
		{
			name:    "an EMBED_BASE_URL embedding credentials fails at startup",
			env:     map[string]string{"RESPONDER": "llm", "EMBED_BASE_URL": "https://user:hunter2@api.voyageai.com/v1"},
			wantErr: true,
		},
		{
			// A silent fallback would make a misconfigured demo look working.
			name:    "an unknown RESPONDER is a startup error, never a fallback",
			env:     map[string]string{"RESPONDER": "ollama"},
			wantErr: true,
		},
		{
			name:    "a malformed LLM_BASE_URL fails at startup",
			env:     map[string]string{"RESPONDER": "llm", "LLM_BASE_URL": "http://[::1"},
			wantErr: true,
		},
		{
			// url.Parse accepts a bare host as an opaque URL, so only a scheme
			// check catches this. Otherwise the service boots healthy and fails
			// every turn as reason="unreachable".
			name:    "a scheme-less LLM_BASE_URL fails at startup",
			env:     map[string]string{"RESPONDER": "llm", "LLM_BASE_URL": "ollama:11434/v1"},
			wantErr: true,
		},
		{
			name:    "a non-URL LLM_BASE_URL fails at startup",
			env:     map[string]string{"RESPONDER": "llm", "LLM_BASE_URL": "not a url at all"},
			wantErr: true,
		},
		{
			name:    "a hostless LLM_BASE_URL fails at startup",
			env:     map[string]string{"RESPONDER": "llm", "LLM_BASE_URL": "http:///v1"},
			wantErr: true,
		},
		{
			// Booting would log the credential and echo it to the browser in
			// an unreachable error.
			name:    "an LLM_BASE_URL embedding credentials fails at startup",
			env:     map[string]string{"RESPONDER": "llm", "LLM_BASE_URL": "https://user:hunter2@api.groq.com/openai/v1"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Nothing listens on port 1, so an llm case that gets past its
			// config checks fails on the embedder probe rather than reaching
			// the compose network this test does not have.
			t.Setenv("EMBED_BASE_URL", "http://127.0.0.1:1/v1")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			r, err := newResponder(context.Background())
			if tt.wantErr {
				if err == nil {
					t.Fatalf("newResponder() error = nil, want an error (got %T)", r)
				}
				return
			}
			if err != nil {
				t.Fatalf("newResponder() error = %v, want nil", err)
			}
			tt.check(t, r)
		})
	}
}

// The rejection message reaches the startup log, which Promtail ships to Loki:
// leaking the credential there defeats the check that rejected it.
func TestValidateBaseURLErrorOmitsCredentials(t *testing.T) {
	err := validateBaseURL("https://user:hunter2@api.groq.com/openai/v1")
	if err == nil {
		t.Fatal("validateBaseURL() error = nil, want an error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error = %q, want the credential omitted", err)
	}
}

// newLLMClient must set ResponseHeaderTimeout — the only thing between a
// wedged provider and a pinned goroutine — and must NOT set Client.Timeout,
// which would kill long generations.
func TestNewLLMClientTimeout(t *testing.T) {
	client := newLLMClient()

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client.Transport = %T, want *http.Transport", client.Transport)
	}
	if transport.ResponseHeaderTimeout != 120*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want %v", transport.ResponseHeaderTimeout, 120*time.Second)
	}
	if client.Timeout != 0 {
		t.Errorf("client.Timeout = %v, want 0 — a whole-request timeout would kill long generations", client.Timeout)
	}
}

// stubRetrievalBackends stands in for Ollama's /embeddings and Qdrant's
// collection info, so RESPONDER=llm can be constructed with neither running.
func stubRetrievalBackends(t *testing.T, dims, points int) (embedURL, qdrantURL string) {
	t.Helper()

	embed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		out := struct {
			Data []item `json:"data"`
		}{}
		for i := range req.Input {
			out.Data = append(out.Data, item{Embedding: make([]float32, dims), Index: i})
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(embed.Close)

	qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"result":{"points_count":%d,"config":{"params":{"vectors":{"size":%d}}}}}`, points, dims)
	}))
	t.Cleanup(qdrant.Close)

	return embed.URL, qdrant.URL
}

// The LLM_* configuration has to reach the responder retrieval delegates to,
// or a hosted provider is configured and never called.
func TestNewResponderConfiguresTheGroundedResponder(t *testing.T) {
	embedURL, qdrantURL := stubRetrievalBackends(t, 768, 412)
	t.Setenv("RESPONDER", "llm")
	t.Setenv("LLM_BASE_URL", "https://api.groq.com/openai/v1/")
	t.Setenv("LLM_MODEL", "llama-3.3-70b-versatile")
	t.Setenv("LLM_API_KEY", "gsk_secret")
	t.Setenv("EMBED_BASE_URL", embedURL)
	t.Setenv("QDRANT_URL", qdrantURL)

	r, err := newResponder(context.Background())
	if err != nil {
		t.Fatalf("newResponder() error = %v, want nil", err)
	}
	retriever, ok := r.(*RetrievingResponder)
	if !ok {
		t.Fatalf("responder = %T, want *RetrievingResponder", r)
	}
	o, ok := retriever.Inner.(*OpenAIResponder)
	if !ok {
		t.Fatalf("Inner = %T, want *OpenAIResponder", retriever.Inner)
	}
	// Trimmed, or request paths become //chat/completions.
	if o.BaseURL != "https://api.groq.com/openai/v1" {
		t.Errorf("BaseURL = %q, want %q", o.BaseURL, "https://api.groq.com/openai/v1")
	}
	if o.Model != "llama-3.3-70b-versatile" {
		t.Errorf("Model = %q, want %q", o.Model, "llama-3.3-70b-versatile")
	}
	if o.APIKey != "gsk_secret" {
		t.Errorf("APIKey = %q, want %q", o.APIKey, "gsk_secret")
	}
	if retriever.TopK != defaultTopK || retriever.Floor != defaultBackgroundFloor {
		t.Errorf("TopK, Floor = %d, %d; want the documented %d, %d", retriever.TopK, retriever.Floor, defaultTopK, defaultBackgroundFloor)
	}
}

// An empty collection presents as a chat that works but knows nothing, which
// is worse than refusing to start.
func TestNewResponderRejectsEmptyAndMismatchedCollections(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		embedURL, qdrantURL := stubRetrievalBackends(t, 768, 0)
		t.Setenv("RESPONDER", "llm")
		t.Setenv("EMBED_BASE_URL", embedURL)
		t.Setenv("QDRANT_URL", qdrantURL)
		if _, err := newResponder(context.Background()); err == nil {
			t.Error("newResponder() error = nil, want a refusal to start against an empty collection")
		}
	})

	t.Run("dimension mismatch", func(t *testing.T) {
		// The collection answers 1536 while the embedder probes 768: same API,
		// another model's space, plausible scores for the wrong passages.
		embed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"data":[{"embedding":` + vectorJSON(768) + `,"index":0}]}`))
		}))
		defer embed.Close()
		qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"result":{"points_count":10,"config":{"params":{"vectors":{"size":1536}}}}}`))
		}))
		defer qdrant.Close()

		t.Setenv("RESPONDER", "llm")
		t.Setenv("EMBED_BASE_URL", embed.URL)
		t.Setenv("QDRANT_URL", qdrant.URL)
		if _, err := newResponder(context.Background()); err == nil {
			t.Error("newResponder() error = nil, want a dimension mismatch refusal")
		}
	})
}

func vectorJSON(dims int) string {
	b, _ := json.Marshal(make([]float32, dims))
	return string(b)
}

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
