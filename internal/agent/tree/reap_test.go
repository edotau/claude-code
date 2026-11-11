package tree

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

// A running claude dispatch hides the session it spawned, by its session id or by cwd + start + opening prompt;
// an unrelated session in the same dir stays.
func TestDropDispatchSessions(t *testing.T) {
	start := time.Date(2026, 10, 18, 3, 9, 0, 0, time.UTC)
	byPrompt := Dispatch{Agent: "claude", cwd: "/repo", started: start, prompt: "Summarize the retry logic"}
	byID := Dispatch{Agent: "claude", sessionID: "sess-id", cwd: "/elsewhere", started: start}
	sessions := []Session{
		{ID: "headless", Cwd: "/repo", Title: "Summarize the retry logic", Updated: start.Add(time.Minute)},
		{ID: "human", Cwd: "/repo", Title: "Add a new feature", Updated: start.Add(time.Minute)},
		{ID: "sess-id", Cwd: "/repo", Title: "other words", Updated: start.Add(time.Minute)},
	}
	var got []string
	for _, s := range dropDispatchSessions(sessions, []Dispatch{byPrompt, byID}) {
		got = append(got, s.ID)
	}
	if len(got) != 1 || got[0] != "human" {
		t.Fatalf("sessions after the dispatch rows took over = %v, want only [human]", got)
	}
}

// A live session whose holder died is marked error and its socket removed; a live holder's socket stays.
func TestReapDeadLiveHolder(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "rp") // short root: a macOS unix socket path caps at 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	mk := func(pid int) agent.Record {
		rec := agent.ClaimRecord("claude", agent.RunOptions{}, time.Now())
		rec.Live, rec.PID = true, pid
		if err := rec.Save(); err != nil {
			t.Fatal(err)
		}
		ln, err := net.Listen("unix", agent.LiveSocket(rec.ID))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		return rec
	}
	alive, dead := mk(os.Getpid()), mk(1<<22+7)
	var out bytes.Buffer
	if code := Reap(false, &out, &out); code != 0 {
		t.Fatalf("Reap = %d: %s", code, out.String())
	}
	if _, err := os.Stat(agent.LiveSocket(dead.ID)); !os.IsNotExist(err) {
		t.Errorf("dead holder's socket must be removed: %v", err)
	}
	if _, err := os.Stat(agent.LiveSocket(alive.ID)); err != nil {
		t.Errorf("live holder's socket must stay: %v", err)
	}
	if got, _ := agent.LoadRecord(dead.ID); got.Status != "error" {
		t.Errorf("dead record status = %s, want error", got.Status)
	}
}

// Given a live-session socket with no dispatch record, Then Reap removes the orphan.
func TestReapRemovesSocketWithoutRecord(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "rp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	const id = "20261108T010101.000-claude"
	if err := os.MkdirAll(filepath.Dir(agent.LiveSocket(id)), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", agent.LiveSocket(id))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var out bytes.Buffer
	if code := Reap(false, &out, &out); code != 0 {
		t.Fatalf("Reap = %d: %s", code, out.String())
	}
	if _, err := os.Stat(agent.LiveSocket(id)); !os.IsNotExist(err) {
		t.Errorf("orphan socket with no record must be removed: %v", err)
	}
}

// Given a stale snapshot of a dead record whose owner restarted it on disk, Then the reaper leaves the live record alone.
func TestReapRecordsSkipsRecordRevivedAfterSnapshot(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := agent.ClaimRecord("claude", agent.RunOptions{}, time.Now())
	rec.PID = 1<<22 + 7
	stale := rec
	rec.PID = os.Getpid()
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	reapRecords([]agent.Record{stale}, false, "reaped", &out, &out)
	got, err := agent.LoadRecord(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "running" || got.PID != os.Getpid() {
		t.Errorf("record = %q pid %d, want the live owner's running record untouched", got.Status, got.PID)
	}
}

// withDispatchFixture points internal/agent's dispatch log at testdata/dispatch for the duration of
// the test, via CLAUDE_CONFIG_DIR (internal/paths.StateDir honors it).
func withDispatchFixture(t *testing.T) {
	t.Helper()
	cfgDir := t.TempDir()
	dst := filepath.Join(cfgDir, "state", "harness", "dispatch")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join("testdata", "dispatch")
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
}

// A dispatch id printed only as the envelope "dispatch_id" (no `ask: dispatch` line) still yields its Dispatch.
func TestBuildDispatchesEnvelopeID(t *testing.T) {
	withDispatchFixture(t)
	path := filepath.Join(t.TempDir(), "agent.jsonl")
	line := `{"type":"user","message":{"content":[{"type":"tool_result","content":"{\"dispatch_id\":\"20261017T231131.948-codex\"}"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	got := buildDispatches(path, "", time.Now(), Activity{})
	if len(got) != 1 || got[0].ID != "20261017T231131.948-codex" {
		t.Fatalf("buildDispatches = %+v, want the envelope dispatch", got)
	}
}
