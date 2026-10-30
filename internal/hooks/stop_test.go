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
	"github.com/edotau/claude-code/internal/settings"
)

// TestStopCombinesBlocksAndStillFormats: through the Stop event, checkpoint and update both block with both
// messages shown, the format group runs anyway, stop_hook_active skips only the checkpoint, and a disabled
// format group leaves the file unformatted while memory still blocks.
func TestStopCombinesBlocksAndStillFormats(t *testing.T) {
	for _, bin := range []string{"gofmt", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not on PATH")
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv(memory.UpdateChildEnv, "")
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
	reset := func() { _ = os.WriteFile(mine, []byte(ugly), 0o644) }
	reset()
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	edit := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/r/a.go"}}]}}` + "\n"
	write := fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":%q}}]}}`+"\n", mine)
	usage := `{"type":"assistant","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":180000,"cache_read_input_tokens":0,"output_tokens":0}}}` + "\n"
	_ = os.WriteFile(tr, []byte(strings.Repeat(edit, 5)+write+usage), 0o600)
	stop := func(id string, active bool) (int, string) {
		p := fmt.Sprintf(`{"session_id":%q,"cwd":%q,"transcript_path":%q,"stop_hook_active":%t}`, id, repo, tr, active)
		var out, errb bytes.Buffer
		code := RunEvent("Stop", false, strings.NewReader(p), &out, &errb)
		return code, errb.String() + out.String()
	}

	code, msg := stop("s1", false)
	if code != ExitBlock {
		t.Fatalf("both hooks block: got %d, want ExitBlock; msg=%q", code, msg)
	}
	if !strings.Contains(msg, "Context usage") || !strings.Contains(msg, "memory update") {
		t.Errorf("combined message missing a part: %q", msg)
	}
	if b, _ := os.ReadFile(mine); string(b) == ugly {
		t.Error("the format group must run even when the stop is blocked")
	}

	code, msg = stop("s2", true)
	if code != ExitBlock || strings.Contains(msg, "Context usage") {
		t.Errorf("stop_hook_active must skip the checkpoint only: code=%d msg=%q", code, msg)
	}

	if err := settings.SaveHarnessHooks(map[string]bool{"format": false}); err != nil {
		t.Skipf("hermetic vault unavailable: %v", err)
	}
	reset()
	if code, msg = stop("s3", false); code != ExitBlock || !strings.Contains(msg, "memory update") {
		t.Errorf("memory must still block with format off: code=%d msg=%q", code, msg)
	}
	if b, _ := os.ReadFile(mine); string(b) != ugly {
		t.Error("a disabled format group must leave the file unformatted")
	}
}
