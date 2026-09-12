package rag

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Source is one indexable file, repo-relative with forward slashes. The path
// becomes both a citation and half of the point ID, so its shape must not vary
// by host OS.
type Source struct {
	Path string
	Kind string // background | source | docs
}

// allowedRoots is an allowlist, not a denylist. A denylist fails open on the
// file nobody thought of, and this index is quoted back to unauthenticated
// visitors. .env has no allowed extension, so no rule has to remember it.
var allowedRoots = []struct {
	prefix string
	kind   string
}{
	{"corpus", "background"},
	{"proto", "source"},
	{"order-service", "source"},
	{"chat-service", "source"},
	{"inventory-service", "source"},
	{"frontend/src", "source"},
	{"docs/superpowers", "docs"},
	{"CLAUDE.md", "docs"},
	{"README.md", "docs"},
}

var allowedExts = map[string]bool{
	".md": true, ".go": true, ".ts": true, ".tsx": true, ".proto": true,
}

// skipDirs are build output and the committed generated packages, matched by
// path rather than by directory name: "chat" and "orders" are ordinary words,
// and a name match would also drop frontend/src/components/chat.
var skipDirs = map[string]bool{
	"node_modules":         true,
	"dist":                 true,
	"frontend/src/gen":     true,
	"chat-service/chat":    true,
	"order-service/orders": true,
}

// Walk selects every file that sits under an allowed root, carries an allowed
// extension, and lies under no skipped directory.
func Walk(root string) ([]Source, error) {
	var out []Source

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}

		if d.IsDir() {
			if skipDirs[rel] || skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !allowedExts[strings.ToLower(path.Ext(rel))] {
			return nil
		}
		if isTestFile(rel) {
			return nil
		}
		if kind, ok := rootKind(rel); ok {
			out = append(out, Source{Path: rel, Kind: kind})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking %s: %w", root, err)
	}
	return out, nil
}

// isTestFile covers both conventions this repo uses. Assertions retrieve
// poorly and crowd real code out of top_k.
func isTestFile(rel string) bool {
	base := path.Base(rel)
	if strings.HasSuffix(base, "_test.go") {
		return true
	}
	for _, infix := range []string{".test.", ".spec."} {
		if strings.Contains(base, infix) {
			return true
		}
	}
	return false
}

// rootKind reports the kind for rel, or false if it is under no allowed root.
// A prefix match is on path segments: "corpus" must not match "corpusdump.md".
func rootKind(rel string) (string, bool) {
	for _, r := range allowedRoots {
		if rel == r.prefix || strings.HasPrefix(rel, r.prefix+"/") {
			return r.kind, true
		}
	}
	return "", false
}

// ReadSource reads one selected file. Kept here so callers never build a path
// from a Source themselves and reach outside root.
func ReadSource(root string, s Source) ([]byte, error) {
	return os.ReadFile(filepath.Join(root, filepath.FromSlash(s.Path)))
}
