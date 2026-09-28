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
