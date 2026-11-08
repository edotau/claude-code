package setup

import (
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/completion"
)

// CleanSpec is the completion node for the clean verb.
func CleanSpec() *completion.Node {
	flag, boolFlag := completion.Plain, completion.BoolFlag
	return &completion.Node{Flags: []completion.Flag{flag("target"), flag("older-than"), boolFlag("dry-run"), boolFlag("yes"), boolFlag("list")}}
}

// InstallSpec is the completion node for the install verb, with its `--setup` modes.
func InstallSpec() *completion.Node {
	flag, boolFlag, valuedFlag := completion.Plain, completion.BoolFlag, completion.ValuedFlag
	return &completion.Node{Flags: []completion.Flag{boolFlag("dry-run"), boolFlag("verbose"), valuedFlag("tools", tools.Names), valuedFlag("setup", Modes)}, Sub: map[string]*completion.Node{"--setup": {Sub: map[string]*completion.Node{
		"adopt":     {Flags: []completion.Flag{flag("from"), flag("into"), boolFlag("dry-run"), flag("label")}},
		"nuke":      {Flags: []completion.Flag{flag("dir"), boolFlag("apply"), boolFlag("keep-memory"), boolFlag("account"), boolFlag("cli")}},
		"workspace": {Args: []func() []string{WorkspaceOps}, Flags: []completion.Flag{flag("with"), boolFlag("dry-run")}},
	}}}}
}
