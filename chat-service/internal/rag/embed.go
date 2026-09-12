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

func (e *OpenAIEmbedder) Dims() int       { return e.dims }
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

	// Ollama answers in request order, but ordering is not part of the API.
	// Trusting arrival order would pair each chunk with another chunk's vector
	// — an index that searches cleanly and answers wrongly.
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
