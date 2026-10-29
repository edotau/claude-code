// Package inflight holds the agent-inflight marker rules shared by the hook (which writes and prunes the
// markers) and the read-only callers (status line, `agents reap`); it imports only paths and proc.
package inflight

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// TTL is the backstop for a marker whose owner cannot be probed (or parsed): it stops counting after this long.
const TTL = 30 * time.Minute

// Idle: a spawn marker whose agent transcript is untouched this long stopped without a SubagentStop.
const Idle = 10 * time.Minute

// Grace: how long a spawn marker may wait for its agent transcript to appear before it is abandoned.
const Grace = 2 * time.Minute

// Prefix starts every marker name: agent-<owner pid>-<id>; the file holds the account key.
const Prefix = "agent-"

// Dir is the machine-wide marker directory.
func Dir() string { return filepath.Join(paths.StateDir(), "agents-inflight") }

// Owner parses the owning pid out of a marker name; ok=false means the entry is not a marker
// (the lock file, a stray), which is NOT the same as a marker whose pid parsed as 0.
func Owner(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, Prefix)
	if !ok {
		return 0, false
	}
	pidText, _, _ := strings.Cut(rest, "-")
	pid, _ := strconv.Atoi(pidText)
	return pid, true
}

// ProbeTrusted says whether proc.Alive answers here (off unix it never does): only a trusted probe may read
// a silent owner as dead, otherwise TTL is the sole backstop.
func ProbeTrusted() bool { return proc.Alive(os.Getpid()) }

// Live is the owner-level rule: a provably dead owner never counts; a batch leg lives with its owner; anything
// else (or any marker when the probe is untrusted) stops counting after TTL. Self's spawn rules live in scan.
func Live(name string, mod, now time.Time, trusted bool) bool {
	owner, ok := Owner(name)
	if !ok {
		return false
	}
	trusted = trusted && owner > 0
	if trusted && !proc.Alive(owner) {
		return false
	}
	id := strings.TrimPrefix(name, Prefix+strconv.Itoa(owner)+"-")
	if trusted && strings.HasPrefix(id, LegPrefix) {
		return true
	}
	return now.Sub(mod) <= TTL
}

// CountOwned counts the live markers owned by owner without removing anything.
func CountOwned(owner int, transcriptDir string, now time.Time) int {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		return 0
	}
	s := &scanner{self: owner, subDir: transcriptDir, now: now, trusted: ProbeTrusted()}
	n := 0
	for _, e := range entries {
		if pid, ok := Owner(e.Name()); !ok || pid != owner {
			continue
		}
		if fi, err := e.Info(); err == nil && s.live(e.Name(), fi.ModTime()) {
			n++
		}
	}
	return n
}

// SubagentDir is where Claude Code writes this session's agent-<id>.jsonl: <project>/<session>/subagents.
func SubagentDir(parentTranscript string) string {
	if parentTranscript == "" {
		return ""
	}
	return filepath.Join(strings.TrimSuffix(parentTranscript, ".jsonl"), "subagents")
}

// SessionOwner is the Claude Code process behind this one: settings.json runs commands via `sh -c`, so skip shells.
func SessionOwner() int {
	pid := os.Getppid()
	for range 8 {
		switch proc.Comm(pid) {
		case "sh", "dash", "bash", "zsh":
			if pp := proc.Parent(pid); pp > 0 {
				pid = pp
				continue
			}
			return 0 // a shell whose parent is unreadable: fail open (0 is never "dead")
		}
		return pid
	}
	return pid
}
