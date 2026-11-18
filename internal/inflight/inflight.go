// Package inflight holds the agent-inflight marker rules shared by the hook (which writes and prunes the
// markers) and the read-only callers (status line, `agents cancel --lost`); it imports only paths and proc.
package inflight

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// ttl is the backstop for a marker whose owner cannot be probed (or parsed): it stops counting after this long.
const ttl = 30 * time.Minute

// Idle: an agent transcript silent this long stopped without a SubagentStop; it outlasts Bash's 10-minute ceiling.
const Idle = 15 * time.Minute

// grace: how long a spawn marker may wait for its agent transcript to appear before it is abandoned.
const grace = 2 * time.Minute

// settle: how long an answered spawn with no agent meta keeps its claim before it counts as refused.
const settle = 10 * time.Second

// Prefix starts every marker name: agent-<owner pid>-<id>; the file holds the account key, then the session transcript.
const Prefix = "agent-"

// Dir is the machine-wide marker directory.
func Dir() string { return filepath.Join(paths.StateDir(), "agents-inflight") }

// Owner parses the owning pid out of a marker name
func Owner(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, Prefix)
	if !ok {
		return 0, false
	}
	pidText, _, _ := strings.Cut(rest, "-")
	pid, _ := strconv.Atoi(pidText)
	return pid, true
}

// probeTrusted says whether proc.Alive answers here (off unix it never does)
func probeTrusted() bool { return proc.Alive(os.Getpid()) }

// ownerLive is the owner-level rule
func ownerLive(name string, mod, now time.Time, trusted bool) bool {
	owner, ok := Owner(name)
	if !ok {
		return false
	}
	trusted = trusted && owner > 0
	if trusted && !proc.Alive(owner) {
		return false
	}
	id := strings.TrimPrefix(name, Prefix+strconv.Itoa(owner)+"-")
	if trusted && strings.HasPrefix(id, DispatchPrefix) {
		return true
	}
	return now.Sub(mod) <= ttl
}

// CountOwned counts the live markers owned by owner without removing anything.
func CountOwned(owner int, now time.Time) int {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		return 0
	}
	live := liveness(now)
	n := 0
	for _, e := range entries {
		if pid, ok := Owner(e.Name()); ok && pid == owner && live(e.Name()) {
			n++
		}
	}
	return n
}

// liveness is the limiter's own marker rule for read-only callers (status line, `agents cancel --lost`); false else.
func liveness(now time.Time) func(name string) bool {
	s := newScanner(now)
	return func(name string) bool {
		if _, ok := Owner(name); !ok {
			return false
		}
		_, transcript, mod, ok := readMarker(Dir(), name)
		return ok && s.live(name, mod, transcript)
	}
}

// RunningToken is the status a dispatch record's bytes must hold to be alive; a prefilter, not a check.
const RunningToken = `"running"`

// DispatchAlive is the shared liveness rule for a dispatch record (status, pid, start time)
func DispatchAlive(status string, pid int, t time.Time) bool {
	if status != "running" {
		return false
	}
	if pid > 0 {
		return proc.Alive(pid)
	}
	return time.Since(t) < time.Minute
}

// SubagentDir is where Claude Code writes this session's agent-<id>.jsonl: <project>/<session>/subagents.
func SubagentDir(parentTranscript string) string {
	if parentTranscript == "" {
		return ""
	}
	return filepath.Join(strings.TrimSuffix(parentTranscript, ".jsonl"), "subagents")
}

var (
	sessionGetppid = os.Getppid
	sessionComm    = proc.Comm
	sessionParent  = proc.Parent
)

// SessionOwner is the Claude Code process behind this one: settings.json runs commands via `sh -c`, so skip shells.
func SessionOwner() int {
	pid := sessionGetppid()
	for depth := 0; ; depth++ {
		if pid <= 1 {
			return 0
		}
		switch sessionComm(pid) {
		case "": // unreadable or already exited: pid may be a dead sh wrapper, so the owner is unknown
			return 0
		case "sh", "dash", "bash", "zsh":
			if depth == 8 {
				return 0
			}
			pid = sessionParent(pid)
		default:
			return pid
		}
	}
}

// LiveDispatches counts owner's dispatch records whose job is alive; an unknown owner (0) counts none.
func LiveDispatches(owner int) int {
	if owner == 0 { // unknown owner: other unowned records aren't ours
		return 0
	}
	n := 0
	liveRecords(func(o int, _ string) {
		if o == owner {
			n++
		}
	})
	return n
}

// LiveRoutes counts the alive dispatch records per overlay confinement ("databricks:qa"); unrouted ones are skipped.
func LiveRoutes() map[string]int {
	out := map[string]int{}
	liveRecords(func(_ int, route string) {
		if route != "" {
			out[route]++
		}
	})
	return out
}

// liveRecords calls fn with the owner and route of every dispatch record whose job is alive.
func liveRecords(fn func(owner int, route string)) {
	dir := paths.DispatchDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil || !bytes.Contains(b, []byte(RunningToken)) {
			continue
		}
		var r struct {
			Status string    `json:"status"`
			PID    int       `json:"pid"`
			Time   time.Time `json:"time"`
			Owner  int       `json:"owner"`
			Route  string    `json:"route"`
		}
		if json.Unmarshal(b, &r) == nil && DispatchAlive(r.Status, r.PID, r.Time) {
			fn(r.Owner, r.Route)
		}
	}
}

// Leaf is one live Agent-tool spawn marker: its session's owner pid and transcript, and its agent once named.
type Leaf struct {
	Owner      int
	ToolUseID  string
	AgentID    string    // "" until the agent's meta.json exists
	Transcript string    // the session transcript the spawn belongs to
	Started    time.Time // the marker's mtime: when the slot was claimed
	Activity   time.Time // the agent transcript's mtime; zero before it exists
}

// LiveLeaves lists the live spawn markers by the limiter's own liveness rule, reading and removing nothing.
func LiveLeaves(now time.Time) []Leaf {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		return nil
	}
	s := newScanner(now)
	seen := map[string]bool{}
	var out []Leaf
	for _, e := range entries {
		owner, ok := Owner(e.Name())
		if !ok {
			continue
		}
		id := strings.TrimPrefix(e.Name(), Prefix+strconv.Itoa(owner)+"-")
		_, transcript, mod, ok := readMarker(Dir(), e.Name())
		if !ok || transcript == "" || strings.HasPrefix(id, DispatchPrefix) || !s.live(e.Name(), mod, transcript) {
			continue
		}
		base := spawnBase(id)
		if key := strconv.Itoa(owner) + "-" + base; !seen[key] {
			seen[key] = true
			out = append(out, newLeaf(s, owner, base, transcript, mod))
		}
	}
	return out
}

func newLeaf(s *scanner, owner int, base, transcript string, mod time.Time) Leaf {
	l := Leaf{Owner: owner, ToolUseID: base, Transcript: transcript, Started: mod}
	if a, ok := s.agents(transcript)[base]; ok {
		l.AgentID = a.id
		if fi, err := os.Stat(filepath.Join(SubagentDir(transcript), "agent-"+a.id+".jsonl")); err == nil {
			l.Activity = fi.ModTime()
		}
	}
	return l
}

// MarkerSession is the session id of a live marker owned by pid (its transcript's base name); "" with none.
func MarkerSession(owner int) string {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if pid, ok := Owner(e.Name()); ok && pid == owner {
			if _, t, _, ok := readMarker(Dir(), e.Name()); ok && t != "" {
				return strings.TrimSuffix(filepath.Base(t), ".jsonl")
			}
		}
	}
	return ""
}
