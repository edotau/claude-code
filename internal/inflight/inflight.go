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

// Idle: a tagged marker whose agent transcript is untouched this long stopped without a SubagentStop.
const Idle = 10 * time.Minute

// Grace: how long a tagged marker may wait for its agent transcript to appear before it is abandoned.
const Grace = 2 * time.Minute

// Prefix starts every marker name: agent-<owner pid>-<id>[@agentid].
const Prefix = "agent-"

// AgentSep joins a marker to the spawned agent's id.
const AgentSep = "@"

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

// Live reports whether the marker still counts: its owner is alive (untagged or unprobeable: inside TTL) and, for
// self's own TAGGED markers, its agent transcript under transcriptDir is neither idle nor long absent.
func Live(name string, mod, now time.Time, self int, transcriptDir string, trusted bool) bool {
	owner, ok := Owner(name)
	if !ok {
		return false
	}
	trusted = trusted && owner > 0
	_, agentID, tagged := strings.Cut(name, AgentSep)
	tagged = tagged && agentID != ""
	// An untagged marker never got its PostToolUse (e.g. an auto-mode-refused spawn): TTL caps it even when owned.
	if (trusted && !proc.Alive(owner)) || ((!trusted || !tagged) && now.Sub(mod) > TTL) {
		return false
	}
	// Only self's markers: a sibling session's transcripts live elsewhere and would read as "missing".
	if tagged && owner == self && transcriptDir != "" {
		transcript, err := os.Stat(filepath.Join(transcriptDir, "agent-"+agentID+".jsonl"))
		missing := os.IsNotExist(err) && now.Sub(mod) > Grace
		idle := err == nil && now.Sub(transcript.ModTime()) > Idle
		return !missing && !idle
	}
	return true
}

// CountOwned counts the live markers owned by owner without removing anything.
func CountOwned(owner int, transcriptDir string, now time.Time) int {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		return 0
	}
	trusted := ProbeTrusted()
	n := 0
	for _, e := range entries {
		if pid, ok := Owner(e.Name()); !ok || pid != owner {
			continue
		}
		if fi, err := e.Info(); err == nil && Live(e.Name(), fi.ModTime(), now, owner, transcriptDir, trusted) {
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
