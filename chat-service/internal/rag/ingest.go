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
