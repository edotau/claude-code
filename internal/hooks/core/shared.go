package core

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

var (
	// GitCommitCmd matches a shell segment that runs `git commit`.
	GitCommitCmd = regexp.MustCompile(`\bgit\b[^;&|\n]*\bcommit\b`)
	// CommitLine matches the `[branch sha] subject` line git prints for a landed commit.
	CommitLine = regexp.MustCompile(`(?m)^\[(?:detached HEAD|[^\]\s]+)(?: \(root-commit\))? ([0-9a-f]{7,40})\] `)
)

// RepoRootOf climbs from dir's nearest existing ancestor to the first dir holding a `.git` entry, or "".
func RepoRootOf(dir string) string {
	for dir != "" {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

// UnderDir reports whether path is dir itself or below it (both already canonical).
func UnderDir(dir, path string) bool {
	return dir != "" && (path == dir || strings.HasPrefix(path, dir+string(filepath.Separator)))
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
	return msg + "  command: " + in.ToolCommand() + "\n" + hint + "\n"
}
