package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The mounted corpus is the only source of real answers. If loadCorpus ignores
// it, every reply silently regresses to the stub and still looks healthy.
func TestLoadCorpusReadsMountedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corpus.md")
	const want = "Aleksi Valta. Go, gRPC, Envoy."
	if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	got, isStub, err := loadCorpus(path)
	if err != nil {
		t.Fatalf("loadCorpus() error = %v, want nil", err)
	}
	if got != want {
		t.Errorf("loadCorpus() = %q, want %q", got, want)
	}
	if isStub {
		t.Error("loadCorpus() reported the stub fallback, want the mounted file")
	}
}

// A fresh clone has no mount. Booting must still work or the demo is broken
// for everyone but the author.
func TestLoadCorpusFallsBackToStubWhenAbsent(t *testing.T) {
	got, isStub, err := loadCorpus(filepath.Join(t.TempDir(), "absent.md"))
	if err != nil {
		t.Fatalf("loadCorpus() error = %v, want nil", err)
	}
	if strings.TrimSpace(got) == "" {
		t.Error("loadCorpus() = empty, want the embedded stub")
	}
	if !isStub {
		t.Error("loadCorpus() did not report the stub fallback, want it reported")
	}
}

// A present-but-unreadable path is a broken mount. Falling back to the stub
// would serve placeholder answers that read as a working demo.
func TestLoadCorpusErrorsOnUnreadablePath(t *testing.T) {
	// A directory is unreadable as a file on every platform.
	if _, _, err := loadCorpus(t.TempDir()); err == nil {
		t.Error("loadCorpus() error = nil, want an error for an unreadable path")
	}
}

// Same reasoning: an empty file is a mount that resolved to nothing.
func TestLoadCorpusErrorsOnEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corpus.md")
	if err := os.WriteFile(path, []byte("   \n\t\n"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	if _, _, err := loadCorpus(path); err == nil {
		t.Error("loadCorpus() error = nil, want an error for an empty corpus")
	}
}

// Without the envelope the model has facts but no instruction to stay inside
// them, which is when it invents employers and dates.
func TestSystemPromptWrapsCorpusInEnvelope(t *testing.T) {
	const corpus = "Worked at ExampleCorp from 2021."
	got := systemPrompt(corpus)

	if !strings.Contains(got, corpus) {
		t.Errorf("systemPrompt() = %q, want it to contain the corpus", got)
	}
	if len(got) <= len(corpus) {
		t.Error("systemPrompt() returned the corpus alone, want instructions around it")
	}
}

// An oversized corpus is truncated by the provider, not rejected, and the
// envelope leads the prompt — so the grounding rules are what get cut.
func TestLoadCorpusErrorsOnOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corpus.md")
	if err := os.WriteFile(path, []byte(strings.Repeat("a", maxCorpusBytes+1)), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	if _, _, err := loadCorpus(path); err == nil {
		t.Error("loadCorpus() error = nil, want an error for an oversized corpus")
	}
}

// A typo in CORPUS_PATH and an unattached mount both surface as ErrNotExist.
// Falling back to the stub there would hide the misconfiguration behind a
// warning the fresh-clone path also emits.
func TestLoadCorpusErrorsWhenParentDirIsMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-mounted", "background.md")
	if _, _, err := loadCorpus(path); err == nil {
		t.Error("loadCorpus() error = nil, want an error for a missing corpus directory")
	}
}
