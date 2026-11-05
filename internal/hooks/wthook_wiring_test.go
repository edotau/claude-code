package hooks

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/wthook"
)

// plainWthookRunners are the worktree-area Native entries registered without a viaStderr wrapper.
var plainWthookRunners = map[string]core.Runner{
	"worktree-create": wthook.Create, "worktree-mode": wthook.Mode, "nested-config-sweep": wthook.NestedConfigSweep,
	"worktree-busy": wthook.Busy, "worktree-claim": wthook.Claim, "worktree-claim-check": wthook.ClaimCheck,
}

// Every worktree-area Native entry is the intended wthook runner: same function, or the same Result as the wrapped helper.
func TestNativeWthookEntriesAreWthookRunners(t *testing.T) {
	for name, want := range plainWthookRunners {
		if reflect.ValueOf(Native[name]).Pointer() != reflect.ValueOf(want).Pointer() {
			t.Errorf("%s: Native entry is not the wthook runner", name)
		}
	}
	payload := `{"hook_event_name":"WorktreeRemove"}`
	var b bytes.Buffer
	want := core.ExitResult(wthook.Remove(core.ParseInput(strings.NewReader(payload)), &b), b.String())
	if got := Native["worktree-remove"](core.ParseInput(strings.NewReader(payload))); !reflect.DeepEqual(got, want) || want.ExitCode != 1 {
		t.Errorf("worktree-remove: Native result %+v, want %+v (exit 1)", got, want)
	}
}

// A failing worktree handler must fail the dispatcher: Claude Code reads exit 0 as "worktree created".
func TestWorktreeFailureKeepsExitCode(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	orig := Native["worktree-create"]
	Native["worktree-create"] = func(*core.Input) core.Result { return core.Result{Stderr: "worktree-create: boom", ExitCode: 1} }
	t.Cleanup(func() { Native["worktree-create"] = orig })

	var out, errb strings.Builder
	code := RunEvent("WorktreeCreate", false, strings.NewReader(`{"hook_event_name":"WorktreeCreate","name":"x"}`), &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "boom") || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q; want exit 1, empty stdout, stderr kept", code, out.String(), errb.String())
	}
}
