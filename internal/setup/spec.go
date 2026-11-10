package setup

import (
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// CleanSpec is the completion node for the clean verb.
func CleanSpec() *helptext.Node {
	flag, boolFlag := helptext.Plain, helptext.BoolFlag
	return &helptext.Node{Flags: []helptext.Flag{flag("target"), flag("older-than"), boolFlag("dry-run"), boolFlag("yes"), boolFlag("list")}}
}

// InstallSpec is the completion node for the install verb, with its `--setup` modes.
func InstallSpec() *helptext.Node {
	flag, boolFlag, valuedFlag := helptext.Plain, helptext.BoolFlag, helptext.ValuedFlag
	return &helptext.Node{Flags: []helptext.Flag{boolFlag("dry-run"), boolFlag("verbose"), valuedFlag("tools", tools.Names), valuedFlag("setup", Modes)}, Sub: map[string]*helptext.Node{"--setup": {Sub: map[string]*helptext.Node{
		"adopt":     {Flags: []helptext.Flag{flag("from"), flag("into"), boolFlag("dry-run"), flag("label")}},
		"nuke":      {Flags: []helptext.Flag{flag("dir"), boolFlag("apply"), boolFlag("keep-memory"), boolFlag("account"), boolFlag("cli")}},
		"workspace": {Sub: map[string]*helptext.Node{"init": {Flags: []helptext.Flag{flag("with"), boolFlag("dry-run")}}, "link": {}, "status": {}}},
	}}}}
}
