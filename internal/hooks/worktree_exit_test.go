package hooks

import (
	"strings"
	"testing"
)

// A failing worktree handler must fail the dispatcher: Claude Code reads exit 0 as "worktree created".
func TestWorktreeFailureKeepsExitCode(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	orig := Native["worktree-create"]
	Native["worktree-create"] = func(*Input) Result { return Result{Stderr: "worktree-create: boom", ExitCode: 1} }
	t.Cleanup(func() { Native["worktree-create"] = orig })

	var out, errb strings.Builder
	code := RunEvent("WorktreeCreate", false, strings.NewReader(`{"hook_event_name":"WorktreeCreate","name":"x"}`), &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "boom") || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q; want exit 1, empty stdout, stderr kept", code, out.String(), errb.String())
	}
}
