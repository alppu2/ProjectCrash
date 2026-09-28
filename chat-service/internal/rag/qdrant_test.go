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
