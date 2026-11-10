package hooks

import (
	"bytes"
	"fmt"
	"github.com/edotau/claude-code/internal/memory"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/memhook/memhooktest"
	"github.com/edotau/claude-code/internal/hooks/quality"
	"github.com/edotau/claude-code/internal/settings"
)

// qualityRunners are the quality-area native entries and the quality runner each must be.
var qualityRunners = map[string]core.Runner{
	"review-simplicity": quality.ReviewSimplicity, "format-stop": quality.StopFormat,
}

// Every quality-area native entry is the intended quality runner.
func TestNativeQualityEntriesAreQualityRunners(t *testing.T) {
	for name, want := range qualityRunners {
		if reflect.ValueOf(native[name]).Pointer() != reflect.ValueOf(want).Pointer() {
			t.Errorf("%s: native entry is not the quality runner", name)
		}
	}
}

// The review-simplicity module renders its advisory as PostToolUse JSON on stdout through the registry.
func TestReviewSimplicityModuleEmitsPostToolUse(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	p := filepath.Join(t.TempDir(), "a.go")
	if err := os.WriteFile(p, []byte("package fx\n\n// one\n// two\nfunc Z() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"hook_event_name":"PostToolUse","tool_name":"Write","cwd":%q,"tool_input":{"file_path":%q}}`,
		filepath.Dir(p), p)
	var out bytes.Buffer
	if code := RunArgs("PostToolUse", []string{"--review-simplicity"}, strings.NewReader(payload), &out, &bytes.Buffer{}); code != core.ExitProceed {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), `"hookEventName":"PostToolUse"`) || !strings.Contains(out.String(), "long_comment") {
		t.Errorf("stdout = %q, want PostToolUse JSON naming long_comment", out.String())
	}
}

// A Workflow call through the dispatch-effort pin step is linted by quality.WorkflowResult: an unpinned leaf gets a model.
func TestDispatchEffortLintsWorkflowViaQuality(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	res := native["dispatch-effort"](core.ParseInput(strings.NewReader(
		`{"tool_name":"Workflow","tool_input":{"script":"agent('x', { label: 'l' });\n"}}`)))
	if got, _ := res.UpdatedInput["script"].(string); !strings.Contains(got, "model: 'sonnet'") {
		t.Errorf("updatedInput.script = %q, want model: 'sonnet' filled", got)
	}
}

// The registry row for dispatch-effort serves the same pin step directly (`claude-code hook PreToolUse --dispatch-effort`).
func TestAgentEffortRegistryDispatches(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code := RunArgs("PreToolUse", []string{"--dispatch-effort"}, strings.NewReader(`{"tool_name":"Agent","tool_input":{}}`), &bytes.Buffer{}, &bytes.Buffer{})
	if code != core.ExitProceed {
		t.Fatalf("RunArgs(PreToolUse, --dispatch-effort) on an Agent payload = %d, want ExitProceed", code)
	}
}

// stopFormatFixture is a git repo holding an unformatted mine.go plus a transcript that triggers the stop gates.
type stopFormatFixture struct {
	mine  string
	ugly  string
	stop  func(id string, active bool) (int, string)
	reset func()
}

// newStopFormatFixture hermetically sets up the Stop event run through the checkpoint, update and format groups.
func newStopFormatFixture(t *testing.T) stopFormatFixture {
	t.Helper()
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
	memhooktest.StubSpawn(t, false)
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
		code := runEvent("Stop", false, strings.NewReader(p), &out, &errb)
		return code, errb.String() + out.String()
	}
	return stopFormatFixture{mine: mine, ugly: ugly, stop: stop, reset: reset}
}

// TestStopCombinesBlocksAndStillFormats: through the Stop event, checkpoint and update both block with both
// messages shown, the format group runs anyway, stop_hook_active skips only the checkpoint, and a disabled
// format group leaves the file unformatted while memory still blocks.
func TestStopCombinesBlocksAndStillFormats(t *testing.T) {
	f := newStopFormatFixture(t)
	mine, ugly, stop, reset := f.mine, f.ugly, f.stop, f.reset
	code, msg := stop("s1", false)
	if code != core.ExitBlock {
		t.Fatalf("the memory update blocks: got %d, want ExitBlock; msg=%q", code, msg)
	}
	if !strings.Contains(msg, "memory update") {
		t.Errorf("block message missing the memory update: %q", msg)
	}
	if b, _ := os.ReadFile(mine); string(b) == ugly {
		t.Error("the format group must run even when the stop is blocked")
	}

	code, msg = stop("s2", true)
	if code != core.ExitBlock || strings.Contains(msg, "Context usage") {
		t.Errorf("stop_hook_active must skip the checkpoint only: code=%d msg=%q", code, msg)
	}

	if err := settings.SaveHarnessHooks(map[string]bool{"format": false}); err != nil {
		t.Skipf("hermetic vault unavailable: %v", err)
	}
	reset()
	if code, msg = stop("s3", false); code != core.ExitBlock || !strings.Contains(msg, "memory update") {
		t.Errorf("memory must still block with format off: code=%d msg=%q", code, msg)
	}
	if b, _ := os.ReadFile(mine); string(b) != ugly {
		t.Error("a disabled format group must leave the file unformatted")
	}
}
