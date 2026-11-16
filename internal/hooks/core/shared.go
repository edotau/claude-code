package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/textutil"
)

// GitCommitCmd matches a shell segment that runs `git commit`; text without "commit" never compiles it.
func GitCommitCmd() *Gated { return gitCommitCmd }

var (
	gitCommitCmd = Gate(`\bgit\b[^;&|\n]*\bcommit\b`, "commit")
	// CommitLine matches the `[branch sha] subject` line git prints for a landed commit.
	CommitLine = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?m)^\[(?:detached HEAD|[^\]\s]+)(?: \(root-commit\))? ([0-9a-f]{7,40})\] `)
	})
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

// Gated is a regexp compiled on first use and tried only on text holding one of its required literals.
type Gated struct {
	src  string
	lits []string
	re   func() *regexp.Regexp
}

// Gate is pat gated on lits: every string pat can match must contain one of them; no lits means ungated.
func Gate(pat string, lits ...string) *Gated {
	return &Gated{src: pat, lits: lits, re: sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(pat) })}
}

// MayMatch reports whether s holds one of the literals, the precondition for any match.
func (g *Gated) MayMatch(s string) bool {
	return len(g.lits) == 0 || slices.ContainsFunc(g.lits, func(l string) bool { return strings.Contains(s, l) })
}

// MatchString is the regexp's MatchString, skipped (and never compiled) when s lacks every literal.
func (g *Gated) MatchString(s string) bool { return g.MayMatch(s) && g.re().MatchString(s) }

// Regexp is the compiled pattern with no gate.
func (g *Gated) Regexp() *regexp.Regexp { return g.re() }

func (g *Gated) String() string { return g.src }

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

var (
	deliveryRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?s)<(agent-message|task-notification)\b.*?</(?:agent-message|task-notification)>` +
			`|Another Claude session sent a message:`)
	})
	reminderRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`) })
)

// AutomatedDelivery reports whether prompt is only an agent hand-back or task notification, with no user-typed text.
func AutomatedDelivery(prompt string) bool {
	p := strings.TrimSpace(prompt)
	if strings.HasPrefix(p, "[SYSTEM NOTIFICATION - NOT USER INPUT]") {
		return true
	}
	rest := deliveryRe().ReplaceAllString(p, "")
	return rest != p && strings.TrimSpace(reminderRe().ReplaceAllString(rest, "")) == ""
}

// seenWait bounds the seen-store lock wait; hooks must not stall, so a held lock fails open.
const seenWait = time.Second

// seenMaxAge is how long a session's seen file may sit untouched before a new session's first write prunes it.
const seenMaxAge = 7 * 24 * time.Hour

// SeenFile is session's once-per-session store of kind (a dir under state/): one line per key already shown.
func SeenFile(kind, session string) string {
	return filepath.Join(paths.StateDir(), filepath.FromSlash(kind), paths.Slug(session, "", 80, "session"))
}

// SeenAdd records key in session's kind store under the file's own lock; false when it was already there.
func SeenAdd(kind, session, key string) (bool, error) {
	file := SeenFile(kind, session)
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return true, err
	}
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		PruneSeen(dir)
	}
	first := true
	ran, err := paths.TryFileLock(file, seenWait, func() error { // the file is its own lock: no .lock sibling
		var rerr error
		first, rerr = appendIfNew(file, key)
		return rerr
	})
	if err == nil && !ran {
		err = errors.New("seen-store lock busy")
	}
	return first, err
}

// PruneSeen removes files in dir untouched for seenMaxAge; their sessions are long gone.
func PruneSeen(dir string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if st, err := e.Info(); err == nil && st.Mode().IsRegular() && time.Since(st.ModTime()) > seenMaxAge {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// appendIfNew appends key to file unless a line already holds it; false when it was already there.
func appendIfNew(file, key string) (bool, error) {
	prev, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, err
	}
	if slices.Contains(strings.Split(string(prev), "\n"), key) {
		return false, nil
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return true, err
	}
	_, werr := f.WriteString(key + "\n")
	return true, errors.Join(werr, f.Close())
}
