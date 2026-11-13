package core

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/textutil"
)

var (
	// GitCommitCmd matches a shell segment that runs `git commit`.
	GitCommitCmd = regexp.MustCompile(`\bgit\b[^;&|\n]*\bcommit\b`)
	// CommitLine matches the `[branch sha] subject` line git prints for a landed commit.
	CommitLine = regexp.MustCompile(`(?m)^\[(?:detached HEAD|[^\]\s]+)(?: \(root-commit\))? ([0-9a-f]{7,40})\] `)
)

// AnchorPath anchors a relative path to cwd; an empty path or cwd leaves it unchanged.
func AnchorPath(path, cwd string) string {
	if path != "" && !filepath.IsAbs(path) && cwd != "" {
		return filepath.Join(cwd, path)
	}
	return path
}

// Git runs git in dir with GIT_* scrubbed: ok=false when git itself refused; err for a missing binary or a timeout.
func Git(ctx context.Context, dir string, args ...string) (lines []string, ok bool, err error) {
	out, err := proc.RunGit(ctx, dir, args...)
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return nil, false, ctx.Err()
	case errors.As(err, &exit):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	return strings.Split(strings.TrimSpace(out), "\n"), true, nil
}

// RelWithin is abs relative to root; false when abs escapes root.
func RelWithin(root, abs string) (string, bool) {
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// NestedIn keeps the worktree paths inside top: the peer scan skips those, never the tree that holds top.
func NestedIn(top string, worktrees []string) []string {
	var out []string
	for _, w := range worktrees {
		if strings.HasPrefix(w, top+string(filepath.Separator)) {
			out = append(out, w)
		}
	}
	return out
}

// CanonPath is absolute and symlink-resolved through the deepest existing ancestor (see paths.CanonDeep).
func CanonPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		p = a
	}
	return paths.CanonDeep(p)
}

// LazyCompile compiles the patterns on first use and caches them.
func LazyCompile(pats ...string) func() []*regexp.Regexp {
	return sync.OnceValue(func() []*regexp.Regexp {
		out := make([]*regexp.Regexp, len(pats))
		for i, p := range pats {
			out[i] = regexp.MustCompile(p)
		}
		return out
	})
}

// CapContext trims merged context to ContextCapChars, cutting the later blocks first.
func CapContext(s string) string {
	return textutil.CapRunes(s, ContextCapChars, "…truncated")
}

// SafetyMessage renders the stderr/reason text; an ask that falls back to a block names the agent.
func SafetyMessage(in *Input, decision Decision, what, pattern, hint string) string {
	word := "BLOCKED"
	if decision == Ask {
		if in.Agent() == AgentClaude {
			word = "ASK"
		} else {
			what += " (no ask decision on " + in.Agent() + ")"
		}
	}
	msg := fmt.Sprintf("safety: %s — %s\n", word, what)
	if pattern != "" {
		msg += "  pattern: " + pattern + "\n"
	}
	return msg + "  command: " + textutil.CapRunes(in.ToolCommand(), 160, "…") + "\n" + hint + "\n"
}
