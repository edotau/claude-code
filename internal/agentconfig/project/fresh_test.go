package project

import (
	"os"
	"path/filepath"
	"testing"
)

// A fresh render passes with a stale or empty live home and touches nothing there; a source that cannot render fails.
func TestCheckFreshRenderIgnoresLiveHomeAndWritesNothingThere(t *testing.T) {
	home, claude := isolate(t)
	writeSource(t, claude, "agents", "demo-agent", "demo-agent")
	writeSource(t, claude, "skills", "demo-skill", "demo-skill")
	projectWriteFile(t, filepath.Join(claude, "CLAUDE.md"), "# rules\n")
	stale := filepath.Join(home, ".gemini", "agents", "demo-agent.md")
	projectWriteFile(t, stale, docsBanner+"\nstale content\n")

	if err := CheckFreshRender(claude); err != nil {
		t.Fatalf("CheckFreshRender on a stale live home: %v", err)
	}
	if b, _ := os.ReadFile(stale); string(b) != docsBanner+"\nstale content\n" {
		t.Errorf("live home file changed: %q", b)
	}
	for _, p := range []string{filepath.Join(claude, "state"), filepath.Join(home, ".agents"), filepath.Join(home, ".codex")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("CheckFreshRender wrote %s", p)
		}
	}

	writeSource(t, claude, "agents", "broken", "other-name")
	if err := CheckFreshRender(claude); err == nil {
		t.Error("CheckFreshRender accepted an agent whose name differs from its file")
	}
}

func BenchmarkCheckFreshRender(b *testing.B) {
	root := b.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o700); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(sourcePath(root, "agents", "demo-agent"), []byte("---\nname: demo-agent\ndescription: Fixture agent.\n---\n\nbody\n"), 0o600); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("# Fixture rules\n"), 0o600); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < b.N; i++ {
		if err := CheckFreshRender(root); err != nil {
			b.Fatal(err)
		}
	}
}
