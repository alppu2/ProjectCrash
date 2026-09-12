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
		t.Errorf("chunk[1] does not repeat chunk[0]'s tail %q", tail)
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
