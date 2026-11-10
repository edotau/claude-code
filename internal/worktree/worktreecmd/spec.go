package worktreecmd

import (
	"os"

	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/worktree"
)

func resolveAgents() []string { return []string{"claude", "codex"} }

func names() []string {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	return worktree.Names(cwd)
}

// Spec is the completion node for the worktree verb tree.
func Spec() *helptext.Node {
	return &helptext.Node{Sub: map[string]*helptext.Node{
		"list":   {Flags: []helptext.Flag{helptext.CompletionFormatFlag()}},
		"create": {Flags: []helptext.Flag{helptext.Plain("branch")}},
		"merge": {Args: []func() []string{names}, Flags: []helptext.Flag{helptext.BoolFlag("dry-run"), helptext.Plain("target"),
			helptext.BoolFlag("no-clean"), {Name: "resolve-agent", Values: resolveAgents}, helptext.BoolFlag("resume"), helptext.Plain("verify"), helptext.BoolFlag("no-verify"), helptext.BoolFlag("full-verify")}},
		"clean":     {Args: []func() []string{names}, Flags: []helptext.Flag{helptext.BoolFlag("dry-run"), helptext.BoolFlag("force")}},
		"clean-all": {Flags: []helptext.Flag{helptext.BoolFlag("dry-run"), helptext.BoolFlag("yes")}},
		"gate":      {Flags: []helptext.Flag{helptext.Plain("dir"), helptext.Plain("scope"), helptext.BoolFlag("lint-check"), helptext.CompletionFormatFlag()}},
	}}
}
