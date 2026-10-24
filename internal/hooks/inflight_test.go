package hooks

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

const inflightPayload = `{"tool_name":"Agent","tool_input":{"prompt":"x"}}`

func TestAgentInflightLockedMarkerMatchesTagLookup(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	owner := sessionOwnerPid()
	if owner == 0 {
		t.Skip("session owner unavailable")
	}
	id := "toolu_locked"
	if code := AgentInflight("pre", strings.NewReader(agentPayload(id)), &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("pre = %d", code)
	}
	want := markerName(owner, id)
	if names := markerNames(t); len(names) != 1 || names[0] != want {
		t.Fatalf("locked branch markers = %v, want [%s]", names, want)
	}
	AgentInflight("tag", strings.NewReader(tagPayload(id, "agent-locked")), &bytes.Buffer{})
	if names := markerNames(t); len(names) != 1 || names[0] != want+"@agent-locked" {
		t.Fatalf("tag lookup did not find the locked branch marker: %v", names)
	}
	AgentInflight("post", strings.NewReader(subagentStopPayload("agent-locked")), &bytes.Buffer{})
	if names := markerNames(t); len(names) != 0 {
		t.Fatalf("tagged marker survived stop: %v", names)
	}
}

func TestSubagentStopCoresUseDecodedInput(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	AgentInflight("pre", strings.NewReader(agentPayload("decoded")), &bytes.Buffer{})
	AgentInflight("tag", strings.NewReader(tagPayload("decoded", "decoded-agent")), &bytes.Buffer{})
	in := ParseInput(strings.NewReader(subagentStopPayload("decoded-agent")))
	in.Raw = nil
	if code := agentInflight("post", in, &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("decoded stop must proceed, got %d", code)
	}
	if names := markerNames(t); len(names) != 0 {
		t.Fatalf("decoded stop did not retire marker: %v", names)
	}
}

// agentPayload builds a pre/post PreToolUse-shaped payload carrying an explicit tool_use_id.
func agentPayload(id string) string {
	return `{"tool_name":"Agent","tool_input":{"prompt":"x"},"tool_use_id":"` + id + `"}`
}

// postFailurePayload is the PostToolUseFailure shape: a bare top-level tool_use_id, no tool_name.
func postFailurePayload(id string) string {
	return `{"tool_use_id":"` + id + `"}`
}

// tagPayload is the PostToolUse "tag" shape: the spawn's tool_use_id plus the agentId its response named.
func tagPayload(id, agentID string) string {
	return `{"tool_use_id":"` + id + `","tool_response":{"agentId":"` + agentID + `"}}`
}

// tagPayloadNoAgentID is a "tag" event whose response carries no agent id — a foreground Agent that
// already finished synchronously, so the marker must retire right here rather than wait for a stop.
func tagPayloadNoAgentID(id string) string {
	return `{"tool_use_id":"` + id + `","tool_response":"finished inline"}`
}

// subagentStopPayload names the stopping subagent by agent_id.
func subagentStopPayload(agentID string) string {
	return `{"hook_event_name":"SubagentStop","agent_id":"` + agentID + `"}`
}

// subagentStopTranscriptPayload names the stopping subagent only via agent_transcript_path — the
// agent_id-absent shape SubagentID's basename fallback exists for.
func subagentStopTranscriptPayload(path string) string {
	return `{"hook_event_name":"SubagentStop","agent_transcript_path":"` + path + `"}`
}

// markerNames returns only agent-* entries in the inflight dir, ignoring .lock and strays.
func markerNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(inflightDir())
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "agent-") {
			names = append(names, e.Name())
		}
	}
	return names
}

// hasMarkerSuffix reports whether any marker name ends with the given suffix (id or "-id").
func hasMarkerSuffix(names []string, suffix string) bool {
	for _, n := range names {
		if strings.HasSuffix(n, suffix) {
			return true
		}
	}
	return false
}

// The cap is a constant, always on: the deep-context tier in rules/workflow/agents.md is 8, and the
// gate is the only machine enforcement of it. Under the cap every dispatch proceeds; the next blocks.
func TestAgentInflightBlocksPastCapAndRetires(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if AgentMaxInflight != 8 {
		t.Fatalf("AgentMaxInflight = %d, want the ≤8 first-party tier", AgentMaxInflight)
	}
	for i := range AgentMaxInflight {
		id := strconv.Itoa(i)
		if code := AgentInflight("pre", strings.NewReader(agentPayload(id)), &bytes.Buffer{}); code != ExitProceed {
			t.Fatalf("dispatch %d under cap must proceed", i)
		}
	}
	var stderr bytes.Buffer
	if code := AgentInflight("pre", strings.NewReader(inflightPayload), &stderr); code != ExitBlock {
		t.Fatalf("dispatch %d at the cap must block", AgentMaxInflight+1)
	}
	if !strings.Contains(stderr.String(), "in flight") || !strings.Contains(stderr.String(), "cap 8") {
		t.Errorf("block message must name the cap: %s", stderr.String())
	}
	// A completion retires one marker → the next dispatch fits again. PostToolUse tags dispatch "0" to
	// the agent it spawned; SubagentStop for that agent id is what actually retires the marker now —
	// SubagentStop carries no tool_use_id (the Agent tool returned its task id long before work ended).
	if code := AgentInflight("tag", strings.NewReader(tagPayload("0", "a1")), &bytes.Buffer{}); code != ExitProceed {
		t.Fatal("tag must proceed")
	}
	if code := AgentInflight("post", strings.NewReader(subagentStopPayload("a1")), &bytes.Buffer{}); code != ExitProceed {
		t.Fatal("post must proceed")
	}
	if code := AgentInflight("pre", strings.NewReader(inflightPayload), &bytes.Buffer{}); code != ExitProceed {
		t.Error("dispatch after a retirement must proceed")
	}
}

// TTL pruning is owner-aware: a stale TAGGED marker of a LIVE owner survives (a long background agent), while a
// stale untagged one (a spawn refused after PreToolUse never gets tagged) and a pid-0 marker are pruned.
func TestAgentInflightTTLPruningIsOwnerAware(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := inflightDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-inflightTTL - time.Minute)

	touch := func(name string) string {
		p := filepath.Join(dir, name)
		if f, err := os.Create(p); err != nil {
			t.Fatal(err)
		} else {
			f.Close()
		}
		if err := os.Chtimes(p, stale, stale); err != nil {
			t.Fatal(err)
		}
		return p
	}
	alive := touch("agent-" + strconv.Itoa(os.Getpid()) + "-alive" + markerAgentSep + "a1")
	untagged := touch("agent-" + strconv.Itoa(os.Getpid()) + "-refused")
	zero := touch("agent-0-zero")

	if n := pruneAndCountMarkers(dir, os.Getpid(), "", time.Now()); n != 1 {
		t.Fatalf("pruneAndCountMarkers = %d, want 1 (only the tagged alive-owner marker survives)", n)
	}
	if _, err := os.Stat(alive); err != nil {
		t.Error("a stale TAGGED marker owned by a LIVE process must not be pruned by TTL alone")
	}
	if _, err := os.Stat(untagged); !os.IsNotExist(err) {
		t.Error("a stale UNTAGGED marker must be pruned by TTL even when its owner is alive")
	}
	if _, err := os.Stat(zero); !os.IsNotExist(err) {
		t.Error("a stale marker with owner pid 0 (unresolvable owner) must be pruned")
	}
}

// A provably dead owner is pruned on sight — no TTL wait: even a marker created moments ago must not
// survive a prune pass once its owning process has already exited.
func TestAgentInflightPrunesDeadOwnerRegardlessOfAge(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := inflightDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn a short-lived process: %v", err)
	}
	deadPid := cmd.Process.Pid
	marker := filepath.Join(dir, "agent-"+strconv.Itoa(deadPid)+"-fresh")
	if f, err := os.Create(marker); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
	if n := pruneAndCountMarkers(dir, os.Getpid(), "", time.Now()); n != 0 {
		t.Errorf("pruneAndCountMarkers = %d, want 0: a dead owner must be pruned regardless of the marker's age", n)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("marker for a dead owner must be removed")
	}
}

func TestAgentInflightTranscriptPruning(t *testing.T) {
	cases := []struct {
		name          string
		markerAge     time.Duration
		transcriptAge time.Duration
		missing       bool
		noParentPath  bool
		untagged      bool
		sibling       bool // owned by another live session: its transcripts are elsewhere, never prune on "missing"
		wantPruned    bool
	}{
		{name: "idle transcript", transcriptAge: 11 * time.Minute, wantPruned: true},
		{name: "fresh transcript", markerAge: 3 * time.Minute, transcriptAge: time.Minute},
		{name: "missing transcript within grace", markerAge: time.Minute, missing: true},
		{name: "missing transcript after grace", markerAge: 3 * time.Minute, missing: true, wantPruned: true},
		{name: "no parent path", transcriptAge: 11 * time.Minute, noParentPath: true},
		{name: "untagged marker", markerAge: 3 * time.Minute, missing: true, untagged: true},
		{name: "sibling session marker", markerAge: 3 * time.Minute, missing: true, sibling: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			transcriptDir := t.TempDir()
			now := time.Now()
			if code := AgentInflight("pre", strings.NewReader(agentPayload("original")), &bytes.Buffer{}); code != ExitProceed {
				t.Fatal("first dispatch must proceed")
			}
			if !c.untagged {
				if code := AgentInflight("tag", strings.NewReader(tagPayload("original", "a1")), &bytes.Buffer{}); code != ExitProceed {
					t.Fatal("tag must proceed")
				}
			}
			names := markerNames(t)
			if len(names) != 1 {
				t.Fatalf("want one original marker, got %v", names)
			}
			marker := filepath.Join(inflightDir(), names[0])
			if c.sibling {
				// Re-own the marker to this test binary: alive, but not the session owner (its parent process).
				renamed := filepath.Join(inflightDir(), strings.Replace(names[0], strconv.Itoa(sessionOwnerPid()), strconv.Itoa(os.Getpid()), 1))
				if err := os.Rename(marker, renamed); err != nil {
					t.Fatal(err)
				}
				marker = renamed
			}
			mtime := now.Add(-c.markerAge)
			if err := os.Chtimes(marker, mtime, mtime); err != nil {
				t.Fatal(err)
			}
			if !c.missing {
				transcript := filepath.Join(transcriptDir, "parent", "subagents", "agent-a1.jsonl")
				if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(transcript, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				mtime := now.Add(-c.transcriptAge)
				if err := os.Chtimes(transcript, mtime, mtime); err != nil {
					t.Fatal(err)
				}
			}
			payload := agentPayload("next")
			if !c.noParentPath {
				payload = fmt.Sprintf(`{"tool_name":"Agent","tool_use_id":"next","transcript_path":%q}`,
					filepath.Join(transcriptDir, "parent.jsonl"))
			}
			if code := AgentInflight("pre", strings.NewReader(payload), &bytes.Buffer{}); code != ExitProceed {
				t.Fatal("next dispatch must proceed")
			}
			wantCount := 2
			if c.wantPruned {
				wantCount = 1
			}
			if names := markerNames(t); len(names) != wantCount {
				t.Errorf("marker count = %d, want %d: %v", len(names), wantCount, names)
			}
			_, err := os.Stat(marker)
			if c.wantPruned && !os.IsNotExist(err) {
				t.Errorf("finished agent's marker must be removed, stat error: %v", err)
			} else if !c.wantPruned && err != nil {
				t.Errorf("active agent's marker must survive: %v", err)
			}
		})
	}
}

// markerOwner's second return distinguishes "not a marker at all" from "a marker whose pid parsed as
// 0" — the two cases the TTL/dead-owner logic in pruneAndCountMarkers must tell apart.
func TestMarkerOwnerParsesPidReportsNonMarkers(t *testing.T) {
	cases := []struct {
		name    string
		wantPid int
		wantOK  bool
	}{
		{"agent-1234-abc", 1234, true},
		{"agent-0-zero", 0, true},
		{".lock", 0, false},
		{"stray", 0, false},
	}
	for _, c := range cases {
		if pid, ok := markerOwner(c.name); pid != c.wantPid || ok != c.wantOK {
			t.Errorf("markerOwner(%q) = (%d, %v), want (%d, %v)", c.name, pid, ok, c.wantPid, c.wantOK)
		}
	}
}

// The dispatch id is model-supplied and lands directly in a filename; markerName must neutralize a
// path-escaping id rather than let it address a file outside the inflight dir.
func TestMarkerNameSanitizesPathEscape(t *testing.T) {
	name := markerName(1234, "../../escape")
	if strings.ContainsAny(name, "/\\") {
		t.Errorf("markerName(%q) produced a path-escaping name: %q", "../../escape", name)
	}
	if !strings.HasPrefix(name, "agent-1234-") {
		t.Errorf("markerName = %q, want the agent-1234- prefix preserved", name)
	}
}

// The Task tool is the other Claude Code spawn shape (older builds); it must count against the same
// cap as Agent — both map through spawnTools, not just one hardcoded name.
func TestAgentInflightCountsTaskSpawns(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	payload := `{"tool_name":"Task","tool_input":{"prompt":"x"}}`
	for i := range AgentMaxInflight {
		if code := AgentInflight("pre", strings.NewReader(payload), &bytes.Buffer{}); code != ExitProceed {
			t.Fatalf("Task spawn %d under cap must proceed", i)
		}
	}
	if code := AgentInflight("pre", strings.NewReader(inflightPayload), &bytes.Buffer{}); code != ExitBlock {
		t.Error("an Agent dispatch must see the Task spawns already in flight — same pool, same cap")
	}
}

func TestAgentInflightIgnoresOtherTools(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	payload := `{"tool_name":"Bash","tool_input":{"command":"ls"}}`
	for range AgentMaxInflight + 1 {
		if code := AgentInflight("pre", strings.NewReader(payload), &bytes.Buffer{}); code != ExitProceed {
			t.Fatal("non-Agent tools must pass through")
		}
	}
	if entries, _ := os.ReadDir(inflightDir()); len(entries) != 0 {
		t.Error("non-Agent tools must not record markers")
	}
}

// A blocked dispatch writes no marker, so retiring its tool_use_id is a no-op: nobody else's marker moves.
func TestAgentInflightPostOnBlockedIDIsNoop(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for i := range AgentMaxInflight {
		id := fmt.Sprintf("under-%d", i)
		if code := AgentInflight("pre", strings.NewReader(agentPayload(id)), &bytes.Buffer{}); code != ExitProceed {
			t.Fatalf("dispatch %s under cap must proceed", id)
		}
	}
	if code := AgentInflight("pre", strings.NewReader(agentPayload("blocked")), &bytes.Buffer{}); code != ExitBlock {
		t.Fatal("dispatch past the cap must block")
	}
	before := markerNames(t)
	if code := AgentInflight("post", strings.NewReader(postFailurePayload("blocked")), &bytes.Buffer{}); code != ExitProceed {
		t.Fatal("post must proceed")
	}
	after := markerNames(t)
	if len(after) != len(before) {
		t.Fatalf("retiring an id that was never written must not change the marker set: before %v after %v", before, after)
	}
	if code := AgentInflight("pre", strings.NewReader(agentPayload("still-blocked")), &bytes.Buffer{}); code != ExitBlock {
		t.Error("the cap must still be full after a no-op retire")
	}
}

// PostToolUseFailure names the exact dispatch that failed; only that marker is removed.
func TestAgentInflightPostRemovesExactMarker(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, id := range []string{"a", "b", "c", "d"} {
		if code := AgentInflight("pre", strings.NewReader(agentPayload(id)), &bytes.Buffer{}); code != ExitProceed {
			t.Fatalf("dispatch %s under cap must proceed", id)
		}
	}
	if code := AgentInflight("post", strings.NewReader(postFailurePayload("b")), &bytes.Buffer{}); code != ExitProceed {
		t.Fatal("post must proceed")
	}
	names := markerNames(t)
	if len(names) != 3 {
		t.Fatalf("want 3 markers after retiring one, got %d: %v", len(names), names)
	}
	if hasMarkerSuffix(names, "-b") {
		t.Errorf("marker for retired id b must be gone: %v", names)
	}
	for _, id := range []string{"a", "c", "d"} {
		if !hasMarkerSuffix(names, "-"+id) {
			t.Errorf("marker for %s must survive: %v", id, names)
		}
	}
}

// A marker whose owner pid is dead is pruned and never counted; a non-marker stray is left alone.
func TestAgentInflightPrunesDeadOwnerIgnoresStray(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := inflightDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dead := filepath.Join(dir, "agent-999999-x")
	if f, err := os.Create(dead); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
	stray := filepath.Join(dir, "stray")
	if f, err := os.Create(stray); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
	for i := range AgentMaxInflight {
		if code := AgentInflight("pre", strings.NewReader(inflightPayload), &bytes.Buffer{}); code != ExitProceed {
			t.Fatalf("dispatch %d must proceed: the dead marker must not count against the cap", i)
		}
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Error("dead-owner marker must be pruned")
	}
	if _, err := os.Stat(stray); err != nil {
		t.Error("non-marker stray must be left alone")
	}
}

// SubagentStop retires ONLY the marker tagged with the stopping agent's id; an untagged sibling, and
// another pid's marker, are both left alone. Supersedes the old "retires owner's oldest" behavior —
// picking "oldest" retired a counted sibling whenever SubagentStop came from an agent that never
// passed the pre gate (the over-retire regression this tag scheme exists to close).
func TestAgentInflightSubagentStopRetiresTaggedMarkerOnly(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := inflightDir()
	if code := AgentInflight("pre", strings.NewReader(agentPayload("older")), &bytes.Buffer{}); code != ExitProceed {
		t.Fatal("first dispatch must proceed")
	}
	if code := AgentInflight("pre", strings.NewReader(agentPayload("newer")), &bytes.Buffer{}); code != ExitProceed {
		t.Fatal("second dispatch must proceed")
	}
	// Only "older" is tagged to agent a1; "newer" stays untagged and must survive a1's SubagentStop.
	if code := AgentInflight("tag", strings.NewReader(tagPayload("older", "a1")), &bytes.Buffer{}); code != ExitProceed {
		t.Fatal("tag must proceed")
	}
	// os.Getpid() (the test process) is live and is not the owner (owner = sessionOwnerPid()), so it
	// stands in for "another live session" whose marker must never be touched by this owner's retire.
	other := "agent-" + strconv.Itoa(os.Getpid()) + "-x@a1"
	if f, err := os.Create(filepath.Join(dir, other)); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
	if code := AgentInflight("post", strings.NewReader(subagentStopPayload("a1")), &bytes.Buffer{}); code != ExitProceed {
		t.Fatal("post must proceed")
	}
	after := markerNames(t)
	if hasMarkerSuffix(after, "-older@a1") {
		t.Errorf("the tagged marker must be retired: %v", after)
	}
	if !hasMarkerSuffix(after, "-newer") {
		t.Errorf("the untagged sibling marker must survive: %v", after)
	}
	if _, err := os.Stat(filepath.Join(dir, other)); err != nil {
		t.Error("another pid's marker must never be retired by this owner's SubagentStop")
	}
}

// A SubagentStop naming an agent id no marker is tagged with must retire NOTHING — the over-retire
// regression: falling back to "the oldest" here would silently drop a counted, still-running dispatch.
func TestAgentInflightSubagentStopUnknownIDIsNoop(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, id := range []string{"a", "b", "c", "d"} {
		if code := AgentInflight("pre", strings.NewReader(agentPayload(id)), &bytes.Buffer{}); code != ExitProceed {
			t.Fatalf("dispatch %s under cap must proceed", id)
		}
	}
	before := markerNames(t)
	if code := AgentInflight("post", strings.NewReader(subagentStopPayload("no-such-agent")), &bytes.Buffer{}); code != ExitProceed {
		t.Fatal("post must proceed")
	}
	after := markerNames(t)
	if len(after) != len(before) {
		t.Fatalf("a SubagentStop naming an unmatched agent id must retire nothing: before %v after %v", before, after)
	}
}

// The "tag" mode renames a dispatch's marker to carry the agent id its spawn response reported, so a
// later SubagentStop can find it by that id.
func TestAgentInflightTagRenamesMarkerToAgentID(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if code := AgentInflight("pre", strings.NewReader(agentPayload("x1")), &bytes.Buffer{}); code != ExitProceed {
		t.Fatal("dispatch must proceed")
	}
	if code := AgentInflight("tag", strings.NewReader(tagPayload("x1", "sub-42")), &bytes.Buffer{}); code != ExitProceed {
		t.Fatal("tag must proceed")
	}
	if names := markerNames(t); !hasMarkerSuffix(names, "-x1@sub-42") {
		t.Errorf("marker must be renamed to carry the agent id: %v", names)
	}
}

// A "tag" event whose response names no agent id is a foreground Agent that already finished
// synchronously — no SubagentStop will ever follow it, so the marker must retire right here.
func TestAgentInflightTagWithoutAgentIDRetiresMarker(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, id := range []string{"a", "b", "c"} {
		if code := AgentInflight("pre", strings.NewReader(agentPayload(id)), &bytes.Buffer{}); code != ExitProceed {
			t.Fatalf("dispatch %s must proceed", id)
		}
	}
	before := markerNames(t)
	if code := AgentInflight("tag", strings.NewReader(tagPayloadNoAgentID("b")), &bytes.Buffer{}); code != ExitProceed {
		t.Fatal("tag must proceed")
	}
	after := markerNames(t)
	if len(after) != len(before)-1 {
		t.Fatalf("a tag reporting no agent id must retire its marker: before %v after %v", before, after)
	}
	if hasMarkerSuffix(after, "-b") {
		t.Errorf("marker b must be retired: %v", after)
	}
	for _, id := range []string{"a", "c"} {
		if !hasMarkerSuffix(after, "-"+id) {
			t.Errorf("marker for %s must survive: %v", id, after)
		}
	}
}

// SubagentStop's agent id may arrive only via agent_transcript_path (agent-<id>.jsonl); SubagentID's
// basename fallback must resolve it so the tagged marker still retires.
func TestAgentInflightSubagentStopUsesTranscriptPathFallback(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if code := AgentInflight("pre", strings.NewReader(agentPayload("t1")), &bytes.Buffer{}); code != ExitProceed {
		t.Fatal("dispatch must proceed")
	}
	if code := AgentInflight("tag", strings.NewReader(tagPayload("t1", "abc123")), &bytes.Buffer{}); code != ExitProceed {
		t.Fatal("tag must proceed")
	}
	if code := AgentInflight("post", strings.NewReader(subagentStopTranscriptPayload("/tmp/agent-abc123.jsonl")), &bytes.Buffer{}); code != ExitProceed {
		t.Fatal("post must proceed")
	}
	if names := markerNames(t); hasMarkerSuffix(names, "@abc123") {
		t.Errorf("transcript-path fallback must resolve the agent id and retire the marker: %v", names)
	}
}

// When the marker-dir lock is held by another racer for the whole inflightLockWait budget, TryFileLock
// returns ran=false — the exact burst condition AgentMaxInflight exists to catch. The fix must still
// enforce the cap on this path (write-then-count) instead of waving every contending dispatch through.
func TestAgentInflightEnforcesCapUnderLockContention(t *testing.T) {
	saved := inflightLockWait
	inflightLockWait = 100 * time.Millisecond
	t.Cleanup(func() { inflightLockWait = saved })
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := inflightDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range AgentMaxInflight {
		if code := AgentInflight("pre", strings.NewReader(agentPayload(strconv.Itoa(i))), &bytes.Buffer{}); code != ExitProceed {
			t.Fatalf("dispatch %d under cap must proceed", i)
		}
	}

	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_, _ = paths.TryFileLock(filepath.Join(dir, ".lock"), 5*time.Second, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)

	var stderr bytes.Buffer
	code := AgentInflight("pre", strings.NewReader(inflightPayload), &stderr)
	if code != ExitBlock {
		t.Fatalf("a dispatch racing a held lock must still be blocked past the cap, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "cap 8") {
		t.Errorf("block message must name the cap: %s", stderr.String())
	}
	if names := markerNames(t); len(names) != AgentMaxInflight {
		t.Errorf("marker count = %d, want it to stay at the cap %d after the over-cap racer backs off: %v",
			len(names), AgentMaxInflight, names)
	}
}

// The tool_use_id in the pre payload appears verbatim in the marker name.
func TestAgentInflightMarkerNameCarriesToolUseID(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if code := AgentInflight("pre", strings.NewReader(agentPayload("zzz123")), &bytes.Buffer{}); code != ExitProceed {
		t.Fatal("dispatch must proceed")
	}
	names := markerNames(t)
	if !hasMarkerSuffix(names, "-zzz123") {
		t.Errorf("marker name must carry the tool_use_id: %v", names)
	}
}

// A SubagentStop retires the slot on its first stop: an agent that then ends through SubagentHandback
// never stops again, and a held slot blocked every dispatch until the idle prune.
func TestSubagentStopRetiresSlot(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	AgentInflight("pre", strings.NewReader(agentPayload("t1")), &bytes.Buffer{})
	AgentInflight("tag", strings.NewReader(tagPayload("t1", "a1")), &bytes.Buffer{})
	if code := SubagentStop(strings.NewReader(subagentStopPayload("a1")), &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("stop must proceed, got %d", code)
	}
	if n := len(markerNames(t)); n != 0 {
		t.Fatalf("a stop must retire the slot, %d markers left", n)
	}
}
