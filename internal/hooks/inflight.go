// `agent-inflight` — PreToolUse on a Claude Code subagent spawn (the Agent/Task tool) counts
// machine-wide in-flight dispatches via marker files and BLOCKS past AgentMaxInflight; markers carry
// the owning process and retire by identity — back-pressure, not a mutex.
package hooks

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// AgentMaxInflight is the machine-wide cap on concurrent Agent dispatches (owner raised 8 → 12 for wide fan-outs).
const AgentMaxInflight = 12

// inflightTTL is the backstop for a marker whose owner cannot be probed (or parsed): a dispatch that
// never reported stops counting after this long. Where the probe works, a dead owner retires it at once.
const inflightTTL = inflight.TTL

// inflightLockWait bounds the wait for the marker-dir lock; the hook itself is killed at 5 s.
var inflightLockWait = 2 * time.Second

const markerPrefix = inflight.Prefix

// markerAgentSep joins a marker to the spawned agent's id; safeMarkerID strips it from both halves,
// so a model-supplied tool_use_id can never forge a second one.
const markerAgentSep = inflight.AgentSep

func inflightDir() string { return inflight.Dir() }

// AgentInflight gates Agent dispatches. mode "pre" prunes dead and stale markers, blocks past the cap,
// and records the new dispatch; mode "tag" binds the marker to the spawned agent's id (or retires it
// when the spawn already answered in the foreground); mode "post" retires a marker: the exact one on
// PostToolUseFailure (a spawn that never ran), else the SubagentStop's own tagged marker on
// SubagentStop. Fail-open on any infra error.
func AgentInflight(mode string, r io.Reader, stderr io.Writer) int {
	return agentInflight(mode, ParseInput(r), stderr)
}

func agentInflight(mode string, in *Input, stderr io.Writer) int {
	dir := inflightDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ExitProceed
	}
	// The session process (claude, codex) is the identity every event shares — never the hook's shell.
	owner := inflight.SessionOwner()
	if owner == 0 {
		return ExitProceed // an unidentifiable owner writes no marker, and must retire none
	}
	switch mode {
	case "tag":
		_, _ = paths.TryFileLock(filepath.Join(dir, ".lock"), inflightLockWait, func() error {
			tagMarker(dir, owner, dispatchID(in), agentIDFromResponse(in))
			return nil
		})
		return ExitProceed
	case "post":
		_, _ = paths.TryFileLock(filepath.Join(dir, ".lock"), inflightLockWait, func() error {
			retireMarker(dir, owner, dispatchID(in), in.SubagentID())
			return nil
		})
		return ExitProceed
	}
	if !spawnTools[in.ToolName] {
		return ExitProceed // not a spawn: nothing to count
	}
	id := dispatchID(in)
	if id == "" {
		// Without a shared dispatch ID, later events cannot match this marker; only pruning can retire it.
		id = strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	code := ExitProceed
	// TryFileLock, not WithFileLock: the latter is fail-open, and counting markers unlocked races a
	// sibling's write into a wrong total. Under contention: write first, then count (see below).
	ran, _ := paths.TryFileLock(filepath.Join(dir, ".lock"), inflightLockWait, func() error {
		n := pruneAndCountMarkers(dir, owner, in.TranscriptPath, time.Now())
		if n >= AgentMaxInflight {
			fmt.Fprintf(stderr, "agent-inflight: %d Agent dispatches already in flight (cap %d, machine-wide) — "+
				"wait for one to finish or run this task sequentially.\n", n, AgentMaxInflight)
			code = ExitBlock
			return nil
		}
		writeMarker(dir, owner, id)
		return nil
	})
	if !ran {
		// Contention IS the burst: write-then-count means every racer sees its peers, so over-cap ones back off.
		writeMarker(dir, owner, id)
		if n := pruneAndCountMarkers(dir, owner, in.TranscriptPath, time.Now()); n > AgentMaxInflight {
			_ = os.Remove(filepath.Join(dir, markerName(owner, id)))
			fmt.Fprintf(stderr, "agent-inflight: %d Agent dispatches already in flight (cap %d, machine-wide) — "+
				"wait for one to finish or run this task sequentially.\n", n-1, AgentMaxInflight)
			return ExitBlock
		}
		return ExitProceed
	}
	return code
}

// SubagentStop retires the stopping subagent's slot on its FIRST stop: an agent that then ends through
// SubagentHandback never stops again, and a held slot would block every new dispatch until the idle
// prune (live: 6 of 8 slots leaked that way on 2026-10-18).
func SubagentStop(r io.Reader, stderr io.Writer) int {
	return agentInflight("post", ParseInput(r), stderr)
}

// spawnTools are Claude Code's subagent-spawn tool names (Agent; Task on older builds).
var spawnTools = map[string]bool{"Agent": true, "Task": true}

// dispatchID identifies ONE spawn across its own pre/post events: Claude Code's tool_use_id.
func dispatchID(in *Input) string { return in.nested("tool_use_id") }

// markerName is agent-<owner pid>-<dispatch id>; the id is the tool_use_id when the event carries one.
func markerName(owner int, id string) string {
	if id == "" {
		id = strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return markerPrefix + strconv.Itoa(owner) + "-" + safeMarkerID(id)
}

// safeMarkerID keeps [A-Za-z0-9_.-]: the id is model-supplied and lands in a path, where `../` escapes the dir.
func safeMarkerID(id string) string {
	return strings.Map(func(r rune) rune {
		if idRune(r) {
			return r
		}
		return '_'
	}, id)
}

// idRune is the marker-id alphabet [A-Za-z0-9_.-].
func idRune(r rune) bool {
	switch {
	case r >= '0' && r <= '9', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r == '_', r == '.', r == '-':
		return true
	}
	return false
}

// agentIDMarkers open the spawned agent's id in a text response; the JSON field is tried first.
var agentIDMarkers = []string{"agentId:", "agentid:", "agent_id:"}

// agentIDFromResponse reads the spawned agent's id out of a PostToolUse tool_response. Empty means
// the Agent ran in the FOREGROUND — its response is the finished work, and no SubagentStop follows.
func agentIDFromResponse(in *Input) string {
	if id := in.nested("tool_response", "agentId"); id != "" {
		return id
	}
	return scanAgentID(in.ToolResponseText())
}

// scanAgentID lifts the id after the first marker; it ends at the first byte an id cannot hold, so a
// trailing quote, comma or newline never rides along.
func scanAgentID(text string) string {
	for _, m := range agentIDMarkers {
		_, after, ok := strings.Cut(text, m)
		if !ok {
			continue
		}
		rest := strings.TrimLeft(after, " \t\"'")
		if end := strings.IndexFunc(rest, func(r rune) bool { return !idRune(r) }); end >= 0 {
			rest = rest[:end]
		}
		if rest != "" {
			return rest
		}
	}
	return ""
}

// tagMarker binds this dispatch's marker to the agent id its spawn reported, so the matching
// SubagentStop retires that ONE marker. No id reported = a foreground Agent that already finished,
// so the marker retires here; no tool_use_id leaves an untagged marker for owner/TTL pruning only.
func tagMarker(dir string, owner int, id, agentID string) {
	if id == "" {
		return
	}
	name := filepath.Join(dir, markerName(owner, id))
	if agentID == "" {
		_ = os.Remove(name)
		return
	}
	_ = os.Rename(name, name+markerAgentSep+safeMarkerID(agentID))
}

func writeMarker(dir string, owner int, id string) {
	if f, err := os.OpenFile(filepath.Join(dir, markerName(owner, id)), os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		_ = f.Close()
	}
}

// ownerDead is true only when the probe itself is trusted: off unix Alive is always false, and
// treating every pid as dead there would silently disable both this and the idle-harvest watcher.
func ownerDead(pid int) bool {
	return pid > 0 && proc.Alive(os.Getpid()) && !proc.Alive(pid)
}

// pruneAndCountMarkers removes markers whose owner is PROVABLY gone — or, when the owner cannot be
// probed at all, whose TTL expired — and, for self's own TAGGED markers, whose agent transcript under
// the session transcript's subagent dir is idle past inflightIdle or absent past inflightGrace (a
// turn-capped agent sends no SubagentStop), and self's UNTAGGED markers the session transcript has
// already answered. Returns the live count. Non-marker entries never count.
func pruneAndCountMarkers(dir string, self int, transcript string, now time.Time) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	probeTrusted := inflight.ProbeTrusted()
	transcriptDir := inflight.SubagentDir(transcript)
	selfPrefix := markerPrefix + strconv.Itoa(self) + "-"
	var session []byte // read lazily: only an untagged self marker needs it
	n := 0
	for _, e := range entries {
		if _, ok := inflight.Owner(e.Name()); !ok {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if id, own := strings.CutPrefix(e.Name(), selfPrefix); own && transcript != "" && !strings.Contains(id, markerAgentSep) {
			if session == nil {
				session, _ = os.ReadFile(transcript)
			}
			if spawnAnswered(session, id) {
				_ = os.Remove(filepath.Join(dir, e.Name()))
				continue
			}
		}
		if !inflight.Live(e.Name(), fi.ModTime(), now, self, transcriptDir, probeTrusted) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
			continue
		}
		n++
	}
	return n
}

// spawnAnswered: the session transcript holds a tool_result for this spawn's tool_use_id, so the spawn already
// ended — refused by auto mode or failed (no PostToolUse ever fires), or a foreground Agent that finished.
func spawnAnswered(session []byte, id string) bool {
	return id != "" && bytes.Contains(session, []byte(`"tool_use_id":"`+id+`"`))
}

// retireMarker removes the marker for this exact dispatch when the event names a tool_use_id (a failed
// spawn wrote it on PreToolUse), else this owner's marker TAGGED with the stopping subagent's id. No
// match retires NOTHING: taking "the oldest" retired a counted sibling whenever the SubagentStop came
// from an agent that never passed the pre gate (a Workflow-tool agent, a teammate).
func retireMarker(dir string, owner int, id, agentID string) {
	if id != "" {
		_ = os.Remove(filepath.Join(dir, markerName(owner, id))) // absent when pre BLOCKED: nothing to retire
		return
	}
	if agentID == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	suffix := markerAgentSep + safeMarkerID(agentID)
	for _, e := range entries {
		if pid, ok := inflight.Owner(e.Name()); !ok || pid != owner {
			continue
		}
		if strings.HasSuffix(e.Name(), suffix) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
			return
		}
	}
}
