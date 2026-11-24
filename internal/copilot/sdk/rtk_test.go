package sdk

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/rtkwrap"
	copilot "github.com/github/copilot-sdk/go"
)

func TestRTKPreToolUseRewritesBashOutsideCodeMode(t *testing.T) {
	bin := hosttest.FakeRTK(t)
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	in := copilot.PreToolUseHookInput{ToolName: "bash", WorkingDirectory: t.TempDir(),
		ToolArgs: map[string]any{"command": "git log --oneline -3", "description": "d"}}
	for mode, want := range map[string]string{"": "rtk git log --oneline -3", "1": ""} {
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
