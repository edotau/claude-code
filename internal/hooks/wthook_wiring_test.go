package hooks

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/wthook"
)

// plainWthookRunners are the worktree-area native entries registered without a viaStderr wrapper.
var plainWthookRunners = map[string]core.Runner{
	"worktree-create": wthook.Create, "worktree-mode": wthook.Mode,
	"worktree-busy": wthook.Busy, "worktree-claim": wthook.Claim, "worktree-claim-check": wthook.ClaimCheck,
}

// Every worktree-area native entry is the intended wthook runner: same function, or the same Result as the wrapped helper.
func TestNativeWthookEntriesAreWthookRunners(t *testing.T) {
	for name, want := range plainWthookRunners {
		if reflect.ValueOf(native[name]).Pointer() != reflect.ValueOf(want).Pointer() {
			t.Errorf("%s: native entry is not the wthook runner", name)
		}
	}
	payload := `{"hook_event_name":"WorktreeRemove"}`
	var b bytes.Buffer
	want := core.ExitResult(wthook.Remove(core.ParseInput(strings.NewReader(payload)), &b), b.String())
	if got := native["worktree-remove"](core.ParseInput(strings.NewReader(payload))); !reflect.DeepEqual(got, want) || want.ExitCode != 1 {
		t.Errorf("worktree-remove: native result %+v, want %+v (exit 1)", got, want)
	}
}

func TestWorktreeCreateDispatchFailureExitsNonzero(t *testing.T) {
	for _, failure := range []string{"panic", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			release, finished := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() { close(release); <-finished })
			spec := specOf("worktree-create")
			spec.Budget = time.Millisecond
			run := func(*core.Input) core.Result {
				defer close(finished)
				if failure == "panic" {
					panic("worktree creation failed")
				}
				<-release
				return core.Result{Stdout: "/unusable/worktree"}
			}
			var out, errb bytes.Buffer
			res, err := Dispatch([]Bound{{Group: "worktree", Spec: spec, Run: run}}, spec.Event, &core.Input{}, &errb)
			if err != nil {
				t.Fatal(err)
			}
			code := core.Emit(core.AgentClaude, spec.Event, res, &out, &errb)
			if code != 1 || out.Len() != 0 {
				t.Fatalf("%s: exit=%d stdout=%q; want exit 1 without a worktree path", failure, code, out.String())
			}
			if !strings.Contains(errb.String(), "worktree-create") {
				t.Fatalf("missing failed handler diagnostic: %q", errb.String())
			}
		})
	}
}

// A failing worktree handler must fail the dispatcher: Claude Code reads exit 0 as "worktree created".
func TestWorktreeFailureKeepsExitCode(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	orig := native["worktree-create"]
	native["worktree-create"] = func(*core.Input) core.Result { return core.Result{Stderr: "worktree-create: boom", ExitCode: 1} }
	t.Cleanup(func() { native["worktree-create"] = orig })

	var out, errb strings.Builder
	code := runEvent("WorktreeCreate", false, strings.NewReader(`{"hook_event_name":"WorktreeCreate","name":"x"}`), &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "boom") || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q; want exit 1, empty stdout, stderr kept", code, out.String(), errb.String())
	}
}
