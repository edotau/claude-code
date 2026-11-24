package sdk

import (
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/rtkwrap"
	"github.com/edotau/claude-code/internal/textutil"
	copilot "github.com/github/copilot-sdk/go"
)

func TestRTKPreToolUseRewritesBashOutsideCodeMode(t *testing.T) {
	bin := hosttest.FakeRTK(t)
	in := copilot.PreToolUseHookInput{ToolName: "bash", WorkingDirectory: t.TempDir(),
		ToolArgs: map[string]any{"command": "git log --oneline -3", "description": "d"}}
	for mode, want := range map[string]string{"": textutil.QuoteSH(bin) + " git log --oneline -3", "on": ""} {
		t.Setenv(rtkwrap.CodemodeEnv, mode)
		out, err := rtkPreToolUse(in, copilot.HookInvocation{})
		if err != nil || out.PermissionDecision != "" {
			t.Fatalf("code mode %q: out = %+v, err = %v", mode, out, err)
		}
		args, _ := out.ModifiedArgs.(map[string]any)
		if got, _ := args["command"].(string); got != want || want != "" && args["description"] != "d" {
			t.Errorf("code mode %q: modified args = %v, want command %q", mode, args, want)
		}
	}
}
