package core

import (
	"slices"
	"strings"
)

// Decision is a hook's verdict on a tool call; the strictest across groups wins.
type Decision int

const (
	Allow Decision = iota
	Ask
	Block
)

// Result is what one handler contributes to its event; the dispatcher merges them and an agent builder renders it.
type Result struct {
	Decision      Decision
	Reason        string
	Context       string // model-visible additionalContext
	SystemMessage string // user-visible note
	UpdatedInput  map[string]any
	Stdout        string // raw stdout: only the worktree group may set it
	Stderr        string
	ExitCode      int // non-zero exit other than 2; honored only from the worktree group
}

// Runner is one handler bound to an event.
type Runner func(*Input) Result

// ExitResult maps a helper's exit code and stderr text to a Result: 2 blocks with the text, other codes pass through.
func ExitResult(code int, stderr string) Result {
	stderr = strings.TrimSpace(stderr)
	switch code {
	case ExitProceed:
		return Result{Stderr: stderr}
	case ExitBlock:
		return Result{Decision: Block, Reason: stderr}
	}
	return Result{Stderr: stderr, ExitCode: code}
}

// JoinContext joins the non-empty context blocks into one additionalContext text.
func JoinContext(blocks ...string) string {
	return strings.Join(slices.DeleteFunc(slices.Clone(blocks), func(b string) bool { return b == "" }), "\n\n")
}
