package hooks

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStopFormatOnlySessionWrites(t *testing.T) {
	if _, err := exec.LookPath("gofmt"); err != nil {
		t.Skip("gofmt not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	ugly := "package x\nfunc  F( ) {}\n"
	mine, theirs := filepath.Join(repo, "mine.go"), filepath.Join(repo, "theirs.go")
	_ = os.WriteFile(mine, []byte(ugly), 0o644)
	_ = os.WriteFile(theirs, []byte(ugly), 0o644)
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	rec := fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":%q}}]}}`+"\n", mine)
	_ = os.WriteFile(tr, []byte(rec), 0o600)
	StopFormat(strings.NewReader(fmt.Sprintf(`{"cwd":%q,"transcript_path":%q}`, repo, tr)))
	if b, _ := os.ReadFile(mine); string(b) == ugly {
		t.Error("session-written file was not formatted")
	}
	if b, _ := os.ReadFile(theirs); string(b) != ugly {
		t.Error("a file the session never wrote was formatted")
	}
}
