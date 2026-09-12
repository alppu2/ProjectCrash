package rag

import (
	"os"
	"path/filepath"
	"testing"
)

// This is the security test for the whole feature. The index is quoted back to
// unauthenticated visitors; a selected .env is a credential leak, not a bug.
func TestWalkNeverSelectsSecrets(t *testing.T) {
	root := fixtureTree(t)

	got, err := Walk(root)
	if err != nil {
		t.Fatalf("Walk() error = %v", err)
	}
	for _, s := range got {
		switch s.Path {
		case ".env", "chat-service/.env", "chat-service/.env.local":
			t.Errorf("Walk() selected %q — the allowlist must exclude it", s.Path)
		}
	}
}

func TestWalkSelectsAllowedRootsAndExtensions(t *testing.T) {
	root := fixtureTree(t)

	got, err := Walk(root)
	if err != nil {
		t.Fatalf("Walk() error = %v", err)
	}
	kinds := map[string]string{}
	for _, s := range got {
		kinds[s.Path] = s.Kind
	}

	want := map[string]string{
		"corpus/background.md":               "background",
		"chat-service/chat.go":               "source",
		"frontend/src/api.ts":                "source",
		"proto/chat.proto":                   "source",
		"docs/superpowers/specs/a-design.md": "docs",
		"CLAUDE.md":                          "docs",
		"README.md":                          "docs",
	}
	for path, wantKind := range want {
		if kinds[path] != wantKind {
			t.Errorf("kind for %q = %q, want %q", path, kinds[path], wantKind)
		}
	}
}

// Generated protobuf, build output and tests would fill top_k with material
// nobody asks about.
func TestWalkSkipsGeneratedAndTests(t *testing.T) {
	root := fixtureTree(t)

	got, err := Walk(root)
	if err != nil {
		t.Fatalf("Walk() error = %v", err)
	}
	for _, s := range got {
		switch s.Path {
		case "chat-service/chat/chat.pb.go",
			"order-service/orders/service.pb.go",
			"frontend/src/gen/chat_pb.ts",
			"frontend/node_modules/pkg/index.ts",
			"chat-service/chat_test.go",
			"docs/scalability-learning-plan.md":
			t.Errorf("Walk() selected %q, which must be skipped", s.Path)
		}
	}
}

// The generated packages are skipped by path, not by directory name: "chat"
// and "orders" are ordinary words, and a name-based skip silently drops the
// frontend's chat components — the code most questions about this repo are
// about.
func TestWalkKeepsNonGeneratedDirsNamedLikeGeneratedOnes(t *testing.T) {
	root := fixtureTree(t)

	got, err := Walk(root)
	if err != nil {
		t.Fatalf("Walk() error = %v", err)
	}
	selected := map[string]bool{}
	for _, s := range got {
		selected[s.Path] = true
	}
	for _, want := range []string{
		"frontend/src/components/chat/ChatPanel.tsx",
		"order-service/orders_handler.go",
	} {
		if !selected[want] {
			t.Errorf("Walk() skipped %q, which is not generated code", want)
		}
	}
}

// Paths become citations and point IDs. A backslash on Windows would produce
// a different ID for the same file than a Linux ingest run.
func TestWalkUsesForwardSlashes(t *testing.T) {
	root := fixtureTree(t)

	got, err := Walk(root)
	if err != nil {
		t.Fatalf("Walk() error = %v", err)
	}
	for _, s := range got {
		if filepath.ToSlash(s.Path) != s.Path {
			t.Errorf("path %q is not slash-separated", s.Path)
		}
	}
}

func fixtureTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := []string{
		".env",
		"chat-service/.env",
		"chat-service/.env.local",
		"corpus/background.md",
		"chat-service/chat.go",
		"chat-service/chat_test.go",
		"chat-service/chat/chat.pb.go",
		"order-service/orders/service.pb.go",
		"order-service/orders_handler.go",
		"frontend/src/api.ts",
		"frontend/src/components/chat/ChatPanel.tsx",
		"frontend/src/gen/chat_pb.ts",
		"frontend/node_modules/pkg/index.ts",
		"proto/chat.proto",
		"docs/superpowers/specs/a-design.md",
		"docs/scalability-learning-plan.md",
		"CLAUDE.md",
		"README.md",
		"secrets.txt",
	}
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	return root
}
