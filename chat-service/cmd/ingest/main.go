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
