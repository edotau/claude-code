package sdk

import (
	"context"
	"maps"

	"github.com/edotau/claude-code/internal/rtkwrap"
	copilot "github.com/github/copilot-sdk/go"
)

// rtkPreToolUse puts a bash call's command in its rtk form; no permission decision, so the permission gate still runs.
func rtkPreToolUse(in copilot.PreToolUseHookInput, _ copilot.HookInvocation) (*copilot.PreToolUseHookOutput, error) {
	args, _ := in.ToolArgs.(map[string]any)
	cmd, _ := args["command"].(string)
	if in.ToolName != "bash" || cmd == "" {
		return &copilot.PreToolUseHookOutput{}, nil
	}
	form := rtkwrap.ForModel(context.Background(), cmd, in.WorkingDirectory)
	if form == cmd {
		return &copilot.PreToolUseHookOutput{}, nil
	}
	out := maps.Clone(args)
	out["command"] = form
	return &copilot.PreToolUseHookOutput{ModifiedArgs: out}, nil
}
