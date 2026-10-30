package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
)

// TestTickRangeSkipsDraftMirror is the regression test: a range tick never lands on an unapproved draft's mirror.
func TestTickRangeSkipsDraftMirror(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "a.go")
	git("commit", "-q", "-m", "a")

	home := plans.Home(repo)
	approved, draft := filepath.Join(home, "2026-01-01-approved.md"), filepath.Join(home, "2026-01-02-draft.md")
	for _, p := range []string{approved, draft} {
		writeTestFile(t, p, "- [ ] 1.1 Task owns: a.go\n")
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(approved, old, old); err != nil { // the draft is newer, so only the skip keeps it out
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(paths.StateDir(), "plans-scratch", "draft.dest"), draft)

	if err := tickRange(context.Background(), repo, "", base+"..HEAD", "", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(approved); !strings.Contains(string(b), "- [x] 1.1") {
		t.Errorf("approved plan not ticked:\n%s", b)
	}
	if b, _ := os.ReadFile(draft); strings.Contains(string(b), "[x]") {
		t.Errorf("draft mirror ticked:\n%s", b)
	}
	if err := tickRange(context.Background(), repo, "", base+"..HEAD", draft, true, io.Discard); err == nil {
		t.Error("--plan naming a draft mirror must be refused")
	}
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
