package quality

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest"
)

func TestStopFormatOnlySessionWrites(t *testing.T) {
	if _, err := exec.LookPath("gofmt"); err != nil {
		t.Skip("gofmt not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := hosttest.Repo(t)
	ugly := "package x\nfunc  F( ) {}\n"
	mine, theirs := filepath.Join(repo, "mine.go"), filepath.Join(repo, "theirs.go")
	_ = os.WriteFile(mine, []byte(ugly), 0o644)
	_ = os.WriteFile(theirs, []byte(ugly), 0o644)
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	rec := fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":%q}}]}}`+"\n", mine)
	_ = os.WriteFile(tr, []byte(rec), 0o600)
	StopFormat(core.ParseInput(strings.NewReader(fmt.Sprintf(`{"cwd":%q,"transcript_path":%q}`, repo, tr))))
	if b, _ := os.ReadFile(mine); string(b) == ugly {
		t.Error("session-written file was not formatted")
	}
	if b, _ := os.ReadFile(theirs); string(b) != ugly {
		t.Error("a file the session never wrote was formatted")
	}
	stamps, _ := filepath.Glob(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "state", "harness", "stop-format", "*.json"))
	if len(stamps) != 1 {
		t.Fatalf("stamps after a format pass: %v", stamps)
	}
	_ = os.Remove(mine)
	_ = os.Remove(theirs)
	StopFormat(core.ParseInput(strings.NewReader(fmt.Sprintf(`{"cwd":%q,"transcript_path":%q}`, repo, tr))))
	if _, err := os.Stat(stamps[0]); !os.IsNotExist(err) {
		t.Error("a clean tree must drop the stale stamp file")
	}
}

// TestChangedFilesFromSubdir: from a repo subdirectory (no `.git` there) dirty paths still resolve cwd-relative.
func TestChangedFilesFromSubdir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := hosttest.Repo(t)
	src := filepath.Join(repo, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(src, "a.go"), []byte("package a\n"), 0o644)
	_ = os.WriteFile(filepath.Join(repo, "top.go"), []byte("package top\n"), 0o644)
	got := changedFiles(t.Context(), src)
	if len(got) != 1 || got[0] != "a.go" {
		t.Fatalf("changedFiles(src) = %v, want [a.go]", got)
	}
}

func TestStopFormatKeepsNewestFiles(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := hosttest.Repo(t)
	var records strings.Builder
	newest := ""
	for i := 0; i <= stopFormatMaxFiles; i++ {
		name := fmt.Sprintf("%03d.go", i)
		hosttest.WriteRel(t, repo, name, "package x\nfunc  F( ) {}\n")
		newest = filepath.Join(repo, name)
		at := time.Now().Add(time.Duration(i-stopFormatMaxFiles) * time.Minute)
		if err := os.Chtimes(newest, at, at); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&records, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":%q}}]}}`+"\n", newest)
	}
	tr := filepath.Join(t.TempDir(), "edits.jsonl")
	if err := os.WriteFile(tr, []byte(records.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	StopFormat(core.ParseInput(strings.NewReader(fmt.Sprintf(`{"cwd":%q,"transcript_path":%q}`, repo, tr))))
	if got, err := os.ReadFile(newest); err != nil || strings.Contains(string(got), "func  F") {
		t.Fatalf("newest file skipped: %q, %v", got, err)
	}
	oldest := filepath.Join(repo, "000.go")
	if got, err := os.ReadFile(oldest); err != nil || !strings.Contains(string(got), "func  F") {
		t.Fatalf("oldest file should be deferred: %q, %v", got, err)
	}
}
