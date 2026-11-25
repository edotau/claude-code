package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/textutil"
)

var defaultHandlerBudget = 5 * time.Second

// BudgetOf is a handler's time budget; leave gates time to render their refusal before the host deadline.
func BudgetOf(s settings.EventSpec) time.Duration {
	budget := s.Budget
	if budget <= 0 {
		budget = defaultHandlerBudget
	}
	if s.Blocks {
		budget -= min(HookMargin/2, budget/2)
	}
	return budget
}

// GitCommitCmd matches a shell segment that runs `git commit`; text without "commit" never compiles it.
func GitCommitCmd() *Gated { return gitCommitCmd }

var (
	gitCommitCmd = Gate(`\bgit\b[^;&|\n]*\bcommit\b`, "commit")
	// CommitLine matches the `[branch sha] subject` line git prints for a landed commit.
	CommitLine = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?m)^\[(?:detached HEAD|[^\]\s]+)(?: \(root-commit\))? ([0-9a-f]{7,40})\] `)
	})
)

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
		if SupportsAsk(in.Agent()) {
			word = "ASK"
		} else {
			what += assets.SafetyNoAsk(in.Agent())
		}
	}
	return assets.SafetyMessage(word, what, pattern, textutil.CapRunes(in.ToolCommand(), 160, "…"), hint)
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
var seenWait = time.Second

// SwapSeenWait sets the seen-store lock wait and returns the func that restores it; a test seam.
func SwapSeenWait(d time.Duration) (restore func()) {
	orig := seenWait
	seenWait = d
	return func() { seenWait = orig }
}

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

// SeenHas reports whether key is in session's kind store, without recording anything.
func SeenHas(kind, session, key string) (bool, error) {
	prev, err := os.ReadFile(SeenFile(kind, session))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return slices.Contains(strings.Split(string(prev), "\n"), key), err
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

// DirtyPaths is every uncommitted path under top.
func DirtyPaths(top string) []string {
	raw, err := proc.RunGit(context.Background(), top, "--no-optional-locks", "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range proc.StatusEntries(raw) {
		out = append(out, e.Path)
	}
	return out
}

// MainSibling is the claude-code binary next to this executable; a var so tests can stub it.
var MainSibling = findMainSibling

func findMainSibling() (string, bool) {
	self, err := os.Executable()
	if err != nil {
		return "", false
	}
	if real, err := filepath.EvalSymlinks(self); err == nil { // a symlink elsewhere still finds its own siblings
		self = real
	}
	full := filepath.Join(filepath.Dir(self), "claude-code")
	return full, paths.IsFile(full)
}
