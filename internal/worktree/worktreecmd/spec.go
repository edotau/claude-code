package worktreecmd

import (
	"os"

	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/worktree"
)

func flag(name string) completion.Flag     { return completion.Flag{Name: name} }
func boolFlag(name string) completion.Flag { return completion.Flag{Name: name, IsBool: true} }

func formatFlag() completion.Flag {
	return completion.Flag{Name: "format", Values: func() []string { return []string{helptext.Text, helptext.JSON} }}
}

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
		"list":   {Flags: []completion.Flag{formatFlag()}},
		"create": {Flags: []completion.Flag{flag("branch")}},
		"merge": {Args: []func() []string{names}, Flags: []completion.Flag{boolFlag("dry-run"), flag("target"),
			boolFlag("no-clean"), {Name: "resolve-agent", Values: resolveAgents}, boolFlag("resume"), flag("verify"), boolFlag("no-verify"), boolFlag("full-verify")}},
		"clean":     {Args: []func() []string{names}, Flags: []completion.Flag{boolFlag("dry-run"), boolFlag("force")}},
		"clean-all": {Flags: []completion.Flag{boolFlag("dry-run"), boolFlag("yes")}},
	}}
}
