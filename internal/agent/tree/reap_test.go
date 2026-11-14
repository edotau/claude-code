package tree

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/transcript"
)

// A running claude dispatch hides the session it spawned (by session id or cwd+start+prompt); an unrelated one stays.
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
	reapSocketConfig(t)
	mk := func(pid int) agent.Record {
		rec := saveReapRecord(t, pid, time.Now())
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
	reapSocketConfig(t)
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

// Given a stale snapshot of a dead record its owner restarted on disk, the reaper leaves the live record alone.
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

// Given a dead running record whose events file holds a complete ok result, Then reap finishes it ok; else it is lost.
func TestReapRecordsFinishesDeadRecordWithSavedOKResult(t *testing.T) {
	reapSocketConfig(t)
	save := func(env *wire.Envelope) agent.Record {
		rec := saveReapRecord(t, deadPID, time.Now().Add(-time.Minute))
		if env != nil {
			events := wire.NewEventLog(rec.EventsPath(), rec.ID)
			_ = events.Emit(wire.Event{Type: wire.EventStart})
			_ = events.Emit(wire.ResultEvent(*env))
			events.Close()
		}
		return rec
	}
	okEnv := wire.NewEnvelope(wire.Result{Agent: "claude", Model: "m", SessionID: "s", Answer: "done"}, nil, "x")
	failEnv := wire.NewEnvelope(wire.Result{Agent: "claude"}, errors.New("boom"), "x")
	done, failed, bare := save(&okEnv), save(&failEnv), save(nil)
	var out bytes.Buffer
	snapshot, _ := agent.ReadRecent(agent.AllRecords)
	if n, code := reapRecords(snapshot, false, "reaped", &out, &out); n != 3 || code != 0 {
		t.Fatalf("reapRecords = %d, %d: %s", n, code, out.String())
	}
	for _, c := range []struct {
		rec             agent.Record
		status, errText string
	}{
		{done, "ok", ""}, {failed, "error", lostError}, {bare, "error", lostError}} {
		got, err := agent.LoadRecord(c.rec.ID)
		if err != nil || got.Status != c.status || got.Error != c.errText {
			t.Errorf("%s = %q %q (%v), want %q %q", c.rec.ID, got.Status, got.Error, err, c.status, c.errText)
		}
	}
	if got, _ := agent.LoadRecord(done.ID); got.Model != "m" || got.SessionID != "s" {
		t.Errorf("finished record = model %q session %q, want them copied from the saved result", got.Model, got.SessionID)
	}
}

// withDispatchFixture points the dispatch log at testdata/dispatch via CLAUDE_CONFIG_DIR (paths.StateDir honors it).
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
		raw = bytes.ReplaceAll(raw, []byte(`"status": "running"`), fmt.Appendf(nil, `"status": "running", "pid": %d`, os.Getpid()))
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
	got := buildDispatches(path, "", time.Now(), Activity{}, transcript.AppendDispatchIDs(nil, map[string]bool{}, []byte(line)))
	if len(got) != 1 || got[0].ID != "20261017T231131.948-codex" {
		t.Fatalf("buildDispatches = %+v, want the envelope dispatch", got)
	}
}

// A relay's pending ask claims only a dispatch started right after its tool_use, not a fresh one from another leaf.
func TestPendingDispatchIgnoresDispatchStartedLongAfterTheAsk(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	askedAt := now.Add(-90 * time.Minute)
	path := filepath.Join(t.TempDir(), "agent-relay.jsonl")
	line := `{"type":"user","timestamp":"` + askedAt.Add(-time.Minute).Format(time.RFC3339) + `"}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	act := Activity{Tool: "Bash", Arg: `claude-code ask --agent codex "x"`, At: askedAt}
	save := func(start time.Time) {
		rec := agent.NewRecord("codex", agent.RunOptions{WorkDir: "/repo"}, start)
		rec.PID = os.Getpid()
		if err := rec.Save(); err != nil {
			t.Fatal(err)
		}
	}
	save(now) // another leaf's dispatch, started 90 minutes after this relay stopped
	if got := buildDispatches(path, "/repo", now, act, nil); len(got) != 0 {
		t.Fatalf("buildDispatches = %+v, want no child for a relay whose ask ran 90m before this dispatch", got)
	}
	save(askedAt.Add(5 * time.Second)) // negative control: the dispatch its own ask launched
	if got := buildDispatches(path, "/repo", now, act, nil); len(got) != 1 || got[0].Matched != "heuristic" {
		t.Fatalf("buildDispatches = %+v, want the one dispatch started right after the ask", got)
	}
}

// orphanFixture stubs the process table, signals and clock; the returned slice collects "TERM|KILL pid" in order.
func orphanFixture(t *testing.T, now time.Time, procs ...proc.Info) *[]string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var sent []string
	stubPidAlive(t, func(int) bool { return true })
	origList, origSig, origNow, origSleep := listProcs, signalGroup, reapNow, reapSleep
	listProcs = func(string) ([]proc.Info, error) { return procs, nil }
	signalGroup = func(pid int, sig syscall.Signal) error {
		name := "TERM"
		if sig == syscall.SIGKILL {
			name = "KILL"
		}
		sent = append(sent, fmt.Sprintf("%s %d", name, pid))
		return nil
	}
	reapNow, reapSleep = func() time.Time { return now }, func(time.Duration) {}
	t.Cleanup(func() {
		listProcs, signalGroup, reapNow, reapSleep = origList, origSig, origNow, origSleep
	})
	return &sent
}

// Only a marked, group-leading, orphaned child past its deadline is stopped (SIGTERM, SIGKILL); the rest stay.
func TestReapStopsOnlyOrphanedDispatchChildren(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	old := now.Add(-26 * time.Hour)
	mk := func(pid, ppid int, mark string, start time.Time) proc.Info {
		return proc.Info{PID: pid, PPID: ppid, PGID: pid, Start: start, Exe: "opencode", Mark: mark}
	}
	orphan := mk(1001, 1, "20261110T100000.000-opencode", old)
	liveParent := mk(1002, 500, "20261110T100000.000-opencode", old)
	byHand := mk(1003, 1, "", old)
	young := mk(1004, 1, "20261111T115000.000-opencode", now.Add(-10*time.Minute))
	notLeader := mk(1005, 1, "20261110T100000.000-opencode", old)
	notLeader.PGID = 77
	dispatcher := proc.Info{PID: 500, PPID: 1, PGID: 500, Start: old, Exe: "claude-code"}
	sent := orphanFixture(t, now, orphan, liveParent, byHand, young, notLeader, dispatcher)

	var out, errw bytes.Buffer
	if code := Reap(true, &out, &errw); code != 0 || len(*sent) != 0 {
		t.Fatalf("dry run: code %d, signals %v, stderr %s; want none sent", code, *sent, errw.String())
	}
	if !strings.Contains(out.String(), "would reap orphan opencode pid 1001") || strings.Count(out.String(), "orphan opencode") != 1 {
		t.Errorf("dry run output = %q, want exactly the orphan 1001 listed", out.String())
	}
	out.Reset()
	if code := Reap(false, &out, &errw); code != 0 {
		t.Fatalf("Reap = %d: %s", code, errw.String())
	}
	if got := strings.Join(*sent, ","); got != "TERM 1001,KILL 1001" {
		t.Errorf("signals = %q, want SIGTERM then SIGKILL to the orphan's group only", got)
	}
	if !strings.Contains(out.String(), "reaped 0 dispatches, 0 markers, 0 sockets, 1 orphans") {
		t.Errorf("summary = %q, want one orphan reaped", out.String())
	}
}

// Given an orphan whose dispatch record is still alive, or younger than its record's own timeout, Then it stays.
func TestOrphanDeadlineFollowsTheDispatchRecord(t *testing.T) {
	now := time.Now()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	live := agent.ClaimRecord("opencode", agent.RunOptions{}, now.Add(-2*time.Hour))
	live.PID = os.Getpid()
	short := agent.ClaimRecord("opencode", agent.RunOptions{Timeout: 2 * time.Minute}, now.Add(-time.Hour))
	short.PID, short.Status = 1<<22+7, "error"
	long := agent.ClaimRecord("opencode", agent.RunOptions{Timeout: 3 * time.Hour}, now.Add(-4*time.Hour))
	long.PID, long.Status = 1<<22+7, "error"
	for _, r := range []agent.Record{live, short, long} {
		if err := r.Save(); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		id   string
		want time.Duration
		ok   bool
	}{{live.ID, 0, false}, {short.ID, 2 * time.Minute, true}, {long.ID, 3 * time.Hour, true},
		{"20200101T000000.000-opencode", agent.DefaultToolsTimeout, true}} {
		if got, ok := orphanDeadline(c.id); got != c.want || ok != c.ok {
			t.Errorf("orphanDeadline(%s) = %v, %v; want %v, %v", c.id, got, ok, c.want, c.ok)
		}
	}
}
