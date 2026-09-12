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
		Embedder: fakeEmbedder{model: "fake", dims: 8, err: errEmbedderDown},
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

var errEmbedderDown = errStub("embedder down")

type errStub string

func (e errStub) Error() string { return string(e) }
