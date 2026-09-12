package rag

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

// ChunkerVersion is hashed with the file bytes. Editing this file leaves file
// bytes identical, so a hash over bytes alone would skip everything and leave
// the index built by the previous chunker.
const ChunkerVersion = "1"

// Rough token budget per chunk and the overlap between neighbours. Tokens are
// approximated as words: precise counting would need the model's tokeniser for
// a bound that only has to be approximately right.
const (
	chunkTargetWords  = 300
	chunkOverlapWords = 50
)

type Chunk struct {
	Text    string
	Line    int    // 1-based, where this chunk starts in the file
	Symbol  string // Go only
	Heading string // markdown only
}

// ChunkMarkdown splits on headings, then packs each section to roughly
// chunkTargetWords with chunkOverlapWords of overlap.
func ChunkMarkdown(src string) []Chunk {
	var out []Chunk
	for _, sec := range splitSections(src) {
		for _, part := range packWords(sec.body, sec.line) {
			text := part.text
			if sec.heading != "" {
				// Prepended, not stored alongside: the embedding is of the
				// text, so context outside it does not reach the vector.
				text = sec.heading + "\n\n" + text
			}
			out = append(out, Chunk{Text: text, Line: part.line, Heading: sec.heading})
		}
	}
	return out
}

type section struct {
	heading string
	body    string
	line    int
}

func splitSections(src string) []section {
	lines := strings.Split(src, "\n")
	var (
		out  []section
		cur  = section{line: 1}
		body strings.Builder
	)
	flush := func() {
		if strings.TrimSpace(body.String()) == "" {
			return
		}
		cur.body = body.String()
		out = append(out, cur)
		body.Reset()
	}

	for i, line := range lines {
		if h, ok := headingText(line); ok {
			flush()
			// The heading's own line, so a citation points at the section.
			cur = section{heading: h, line: i + 1}
			continue
		}
		// Preamble before the first heading falls into cur, which keeps line 1.
		body.WriteString(line)
		body.WriteString("\n")
	}
	flush()
	return out
}

func headingText(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "#") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimLeft(trimmed, "#")), true
}

type part struct {
	text string
	line int
}

// packWords fills chunks to chunkTargetWords, carrying chunkOverlapWords from
// the previous chunk into the next.
func packWords(body string, startLine int) []part {
	words := strings.Fields(body)
	if len(words) == 0 {
		return nil
	}
	if len(words) <= chunkTargetWords {
		return []part{{text: strings.TrimSpace(body), line: startLine}}
	}

	var out []part
	for start := 0; start < len(words); start += chunkTargetWords - chunkOverlapWords {
		end := min(start+chunkTargetWords, len(words))
		out = append(out, part{text: strings.Join(words[start:end], " "), line: startLine})
		if end == len(words) {
			break
		}
	}
	return out
}

// ChunkGo chunks per top-level declaration, carrying the package clause and
// the declaration's doc comment. An error means the file is skipped, not that
// the run fails.
func ChunkGo(filename, src string) ([]Chunk, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", filename, err)
	}

	pkg := "package " + file.Name.Name
	lines := strings.Split(src, "\n")

	var out []Chunk
	for _, decl := range file.Decls {
		// Imports are not answers, and one chunk per import block would crowd
		// real declarations out of top_k.
		if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
			continue
		}

		start := fset.Position(declStart(decl)).Line
		end := fset.Position(decl.End()).Line
		if start < 1 || end > len(lines) {
			continue
		}

		body := strings.Join(lines[start-1:end], "\n")
		out = append(out, Chunk{
			Text:   pkg + "\n\n" + body,
			Line:   fset.Position(decl.Pos()).Line,
			Symbol: declSymbol(decl),
		})
	}
	return out, nil
}

// declStart includes the doc comment: the reasoning above a declaration is
// usually what a question about it matches on.
func declStart(decl ast.Decl) token.Pos {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Doc != nil {
			return d.Doc.Pos()
		}
	case *ast.GenDecl:
		if d.Doc != nil {
			return d.Doc.Pos()
		}
	}
	return decl.Pos()
}

// declSymbol names the chunk for the citation. A multi-name const or var block
// is named for its first entry; the whole block is one chunk either way.
func declSymbol(decl ast.Decl) string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv != nil && len(d.Recv.List) > 0 {
			return receiverName(d.Recv.List[0].Type) + "." + d.Name.Name
		}
		return d.Name.Name
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				return s.Name.Name
			case *ast.ValueSpec:
				if len(s.Names) > 0 {
					return s.Names[0].Name
				}
			}
		}
	}
	return ""
}

func receiverName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return receiverName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr: // generic receiver
		return receiverName(t.X)
	}
	return ""
}
