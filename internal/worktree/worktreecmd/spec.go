package worktreecmd

import (
	"os"

	"github.com/edotau/claude-code/internal/completion"
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
func Spec() *completion.Node {
	return &completion.Node{Sub: map[string]*completion.Node{
		"list":   {Flags: []completion.Flag{completion.FormatFlag()}},
		"create": {Flags: []completion.Flag{completion.Plain("branch")}},
		"merge": {Args: []func() []string{names}, Flags: []completion.Flag{completion.BoolFlag("dry-run"), completion.Plain("target"),
			completion.BoolFlag("no-clean"), {Name: "resolve-agent", Values: resolveAgents}, completion.BoolFlag("resume"), completion.Plain("verify"), completion.BoolFlag("no-verify"), completion.BoolFlag("full-verify")}},
		"clean":     {Args: []func() []string{names}, Flags: []completion.Flag{completion.BoolFlag("dry-run"), completion.BoolFlag("force")}},
		"clean-all": {Flags: []completion.Flag{completion.BoolFlag("dry-run"), completion.BoolFlag("yes")}},
	}}
}
