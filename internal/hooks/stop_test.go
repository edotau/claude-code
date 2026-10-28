package hooks

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/memory"
)

// TestStopCombinesBlocksAndStillFormats: checkpoint and harvest both block with both messages shown, the
// formatter runs anyway, and stop_hook_active skips only the checkpoint.
func TestStopCombinesBlocksAndStillFormats(t *testing.T) {
	for _, bin := range []string{"gofmt", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not on PATH")
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.HarvestChildEnv, "")
	for _, k := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL"} {
		t.Setenv(k, "")
	}
	stubSpawn(t, false)
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	ugly := "package x\nfunc  F( ) {}\n"
	mine := filepath.Join(repo, "mine.go")
	_ = os.WriteFile(mine, []byte(ugly), 0o644)
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	edit := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/r/a.go"}}]}}` + "\n"
	write := fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":%q}}]}}`+"\n", mine)
	usage := `{"type":"assistant","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":180000,"cache_read_input_tokens":0,"output_tokens":0}}}` + "\n"
	_ = os.WriteFile(tr, []byte(strings.Repeat(edit, 5)+write+usage), 0o600)
	payload := func(id string, active bool) string {
		return fmt.Sprintf(`{"session_id":%q,"cwd":%q,"transcript_path":%q,"stop_hook_active":%t}`, id, repo, tr, active)
	}

	var errb bytes.Buffer
	if got := Stop(strings.NewReader(payload("s1", false)), &errb); got != ExitBlock {
		t.Fatalf("both hooks block: got %d, want ExitBlock", got)
	}
	if msg := errb.String(); !strings.Contains(msg, "Context usage") || !strings.Contains(msg, "memory update") {
		t.Errorf("combined message missing a part: %q", msg)
	}
	if b, _ := os.ReadFile(mine); string(b) == ugly {
		t.Error("the formatter must run even when the stop is blocked")
	}

	errb.Reset()
	if got := Stop(strings.NewReader(payload("s2", true)), &errb); got != ExitBlock || strings.Contains(errb.String(), "Context usage") {
		t.Errorf("stop_hook_active must skip the checkpoint only: code=%d msg=%q", got, errb.String())
	}
}
