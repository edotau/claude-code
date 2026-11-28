package tree

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/dev/transcript"
	"github.com/edotau/claude-code/internal/platform/contract"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
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
	failEnv := wire.NewEnvelope(wire.Result{Agent: "claude", Usage: wire.Usage{InputTokens: 7, OutputTokens: 3}}, errors.New("boom"), "x")
	done, failed, bare := save(&okEnv), save(&failEnv), save(nil)
	kept := saveReapRecord(t, deadPID, time.Now().Add(-time.Minute))
	kept.Error, kept.Usage, kept.DurationMs = "saved reason", &wire.Usage{InputTokens: 5}, 1234
	if err := kept.Save(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	snapshot, _ := agent.RunningRecent(agent.AllRecords)
	if n, code := reapRecords(snapshot, false, "reaped", &out, &out); n != 4 || code != 0 {
		t.Fatalf("reapRecords = %d, %d: %s", n, code, out.String())
	}
	for _, c := range []struct {
		rec             agent.Record
		status, errText string
	}{
		{done, "ok", ""}, {failed, "error", "boom"}, {bare, "error", agent.LostError}, {kept, "error", "saved reason"}} {
		got, err := agent.LoadRecord(c.rec.ID)
		if err != nil || got.Status != c.status || got.Error != c.errText {
			t.Errorf("%s = %q %q (%v), want %q %q", c.rec.ID, got.Status, got.Error, err, c.status, c.errText)
		}
	}
	if got, _ := agent.LoadRecord(failed.ID); got.Usage == nil || got.Usage.InputTokens != 7 {
		t.Errorf("failed record usage = %+v, want the saved envelope's", got.Usage)
	}
	if got, _ := agent.LoadRecord(kept.ID); got.Usage == nil || got.Usage.InputTokens != 5 || got.DurationMs != 1234 {
		t.Errorf("kept record = usage %+v, %dms, want its own outcome untouched", got.Usage, got.DurationMs)
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
	got := buildDispatches(path, "", time.Now(), Activity{}, (&agent.RecordIndex{}).Load(transcript.AppendDispatchIDs(nil, map[string]bool{}, []byte(line))))
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
	rec := agent.NewRecord("opencode", agent.RunOptions{Tools: true}, old)
	rec.ID, rec.Status, rec.PID = orphan.Mark, "error", deadPID
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}

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

// Given a leader that exits on SIGTERM while a member of its group stays, Then the group still gets SIGKILL.
func TestReapKillsGroupMembersAfterLeaderExits(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	old := now.Add(-26 * time.Hour)
	mark := "20261110T100000.000-opencode"
	leader := proc.Info{PID: 1001, PPID: 1, PGID: 1001, Start: old, Exe: "opencode", Mark: mark}
	member := proc.Info{PID: 1011, PPID: 1001, PGID: 1001, Start: old, Exe: "sleep"}
	sent := orphanFixture(t, now, leader, member)
	stubPidAlive(t, func(int) bool { return false })
	rec := agent.NewRecord("opencode", agent.RunOptions{Tools: true}, old)
	rec.ID, rec.Status, rec.PID = mark, "error", deadPID
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	var out, errw bytes.Buffer
	if code := Reap(false, &out, &errw); code != 0 {
		t.Fatalf("Reap = %d: %s", code, errw.String())
	}
	if got := strings.Join(*sent, ","); got != "TERM 1001,KILL 1001" {
		t.Errorf("signals = %q, want SIGKILL to the group while member 1011 outlives its leader", got)
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
		{"20200101T000000.000-opencode", orphanMissingTimeout, true}} {
		if got, ok := orphanDeadline(c.id); got != c.want || ok != c.ok {
			t.Errorf("orphanDeadline(%s) = %v, %v; want %v, %v", c.id, got, ok, c.want, c.ok)
		}
	}
}

// writeBenchDispatchLog writes n records (the running newest, the rest ok) without Save's per-record prune.
func writeBenchDispatchLog(b testing.TB, n, running int) []string {
	b.Helper()
	dir := paths.DispatchDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		b.Fatal(err)
	}
	base := agent.NewRecord("claude", agent.RunOptions{
		Prompt: "Summarize the retry logic", WorkDir: "/repo", Model: "claude-sonnet-4-5",
	}, time.Now())
	ids := make([]string, 0, n)
	write := func(rec agent.Record) {
		raw, err := json.MarshalIndent(rec, "", "  ") // the bytes Record.Save's encode would write
		if err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rec.ID+".json"), raw, 0o600); err != nil {
			b.Fatal(err)
		}
		ids = append(ids, rec.ID)
	}
	finished := time.Now().Add(-14 * 24 * time.Hour)
	for i := range n - running {
		rec := base
		rec.Time = finished.Add(time.Duration(i) * 7 * time.Minute)
		rec.ID = rec.Time.UTC().Format("20060102T150405.000") + "-claude"
		rec.Status, rec.DurationMs = "ok", 9000+int64(i%700)
		rec.Provider, rec.Usage = "subscription", &wire.Usage{InputTokens: 12000, OutputTokens: 800}
		write(rec)
	}
	fresh := time.Now().Add(-time.Duration(running) * 100 * time.Millisecond)
	for i := range running { // pid-less and under a minute old: DispatchAlive's clock path, live on every OS
		rec := base
		rec.Time = fresh.Add(time.Duration(i) * 100 * time.Millisecond)
		rec.ID = rec.Time.UTC().Format("20060102T150405.000") + "-claude"
		write(rec)
	}
	return ids
}

// BenchmarkReap2000DispatchRecords is Reap over a full dispatch log: the RunningRecent full scan the profile named.
func BenchmarkReap2000DispatchRecords(b *testing.B) {
	b.Setenv("CLAUDE_CONFIG_DIR", b.TempDir())
	orig := listProcs // the process table is not the dispatch-log scan this benchmark measures
	listProcs = func(string) ([]proc.Info, error) { return nil, nil }
	b.Cleanup(func() { listProcs = orig })
	writeBenchDispatchLog(b, 2000, 100)
	b.ReportAllocs()
	for b.Loop() {
		if code := Reap(true, io.Discard, io.Discard); code != 0 {
			b.Fatalf("Reap = %d, want 0", code)
		}
	}
}

func TestReapOrphanOwnTimeout(t *testing.T) {
	orig := orphanMissingTimeout
	orphanMissingTimeout = 21 * time.Minute
	t.Cleanup(func() { orphanMissingTimeout = orig })
	now := time.Now()
	for _, tc := range []struct {
		name                string
		timeout, age        time.Duration
		missing, live, want bool
	}{
		{"inside explicit", 30 * time.Minute, 20 * time.Minute, false, false, false},
		{"past explicit", 30 * time.Minute, 32 * time.Minute, false, false, true},
		{"default inside", 0, 9 * time.Minute, false, false, false},
		{"default past", 0, 17 * time.Minute, false, false, true},
		{"pruned timeout unknown", 30 * time.Minute, 20 * time.Minute, true, false, false},
		{"pruned at ceiling", 0, orphanMissingTimeout + orphanSlack, true, false, false},
		{"pruned past ceiling", 0, orphanMissingTimeout + orphanSlack + time.Second, true, false, true},
		{"live dispatcher", 30 * time.Minute, time.Hour, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			rec := agent.ClaimRecord("opencode", agent.RunOptions{Tools: true, Timeout: tc.timeout}, now.Add(-tc.age))
			rec.PID, rec.Status = deadPID, "error"
			if tc.live {
				rec.PID, rec.Status = os.Getpid(), "running"
			}
			if err := rec.Save(); err != nil {
				t.Fatal(err)
			}
			if tc.missing {
				if err := os.Remove(filepath.Join(paths.DispatchDir(), rec.ID+".json")); err != nil {
					t.Fatal(err)
				}
			}
			p := proc.Info{PID: 1001, PPID: 1, PGID: 1001, Start: now.Add(-tc.age), Exe: "opencode", Mark: rec.ID}
			got := orphanedChildren([]proc.Info{p}, now)
			if (len(got) == 1) != tc.want {
				t.Fatalf("orphans=%v, want reap=%v", got, tc.want)
			}
		})
	}
}

func TestContractNoteEmptyForPlainDispatch(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if got := contractNote("d-none"); got != "" {
		t.Fatalf("contractNote = %q; want empty", got)
	}
}

func TestContractNoteSumsSubtree(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const owner, child = "20261124T100000.000-pi", "20261124T100001.000-pi"
	c, ps := contract.Open(contract.Scope{Unit: "func x", Kind: contract.KindFunction, Own: []string{"a.go"}},
		contract.Party{Role: contract.RoleOwner, Agent: "pi"}, contract.Limits{Depth: 1, Tokens: 100, Leaves: 6},
		contract.Facts{Fleet: true})
	if c, err := c.Start(owner); len(ps) > 0 || err != nil || contract.Save(c) != nil {
		t.Fatalf("contract: %v %v", ps, err)
	}
	for id, parent := range map[string]string{owner: "", child: owner} {
		r := agent.Record{ID: id, Time: time.Now().Add(-time.Minute), Agent: "pi", Status: "ok", Parent: parent,
			Usage: &wire.Usage{OutputTokens: 20}}
		if err := r.Save(); err != nil {
			t.Fatal(err)
		}
	}
	if got := contractNote(owner); !strings.Contains(got, "40% budget") {
		t.Fatalf("contractNote = %q; want the owner and child summed", got)
	}
}

// A record still marked running whose process is dead reads "lost" like the dispatches list, never "quiet".
func TestDeadRunningDispatchReadsLost(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now()
	rec := agent.ClaimRecord("codex", agent.RunOptions{}, now.Add(-20*time.Minute))
	rec.Status, rec.PID = "running", deadPID
	d := dispatchFromRecord(rec, "", now)
	if row := dispatchRow(d, now); d.Quiet || d.Status != "lost" || strings.Contains(row, "quiet") || !strings.Contains(row, "lost") {
		t.Errorf("dead running dispatch: quiet=%v status=%q row=%q, want lost", d.Quiet, d.Status, row)
	}
}

// A last signal in the future (clock moved back) shows a zero age, never a negative one.
func TestFutureSignalClampsToZero(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now()
	rec := agent.ClaimRecord("codex", agent.RunOptions{}, now.Add(-time.Minute))
	rec.Status, rec.PID = "running", os.Getpid()
	if err := os.WriteFile(rec.EventsPath(), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := now.Add(time.Hour)
	if err := os.Chtimes(rec.EventsPath(), future, future); err != nil {
		t.Fatal(err)
	}
	if got := sinceCell(dispatchFromRecord(rec, "", now), now); strings.HasPrefix(got, "-") {
		t.Errorf("since = %q, want no negative age", got)
	}
}

// A running dispatch's last sign of life is its newest event/log write; past quietAfter its since cell reads "⚠7m".
func TestRunningDispatchSignalAndQuiet(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now()
	rec := agent.ClaimRecord("codex", agent.RunOptions{}, now.Add(-20*time.Minute))
	rec.Status, rec.PID = "running", os.Getpid()
	if err := os.WriteFile(rec.EventsPath(), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		wrote time.Duration
		quiet bool
		want  string
	}{{-30 * time.Second, false, "30s"}, {-7 * time.Minute, true, "⚠7m"}} {
		if err := os.Chtimes(rec.EventsPath(), now.Add(c.wrote), now.Add(c.wrote)); err != nil {
			t.Fatal(err)
		}
		d := dispatchFromRecord(rec, "", now)
		if got := sinceCell(d, now); d.Quiet != c.quiet || d.SignalSource != "events" || got != c.want {
			t.Errorf("wrote %v ago: quiet=%v source=%q since=%q, want quiet=%v with %q", c.wrote, d.Quiet, d.SignalSource, got, c.quiet, c.want)
		}
	}
}

// dispatchRow is a dispatch's cells joined, for substring checks.
func dispatchRow(d Dispatch, now time.Time) string { return strings.Join(jobCells(d, now), " ") }

// sinceCell is a dispatch row's time-since-last-event cell.
func sinceCell(d Dispatch, now time.Time) string { return jobCells(d, now)[6] }

// A running row shows agent, model · route, since and elapsed, no "↑- ↓-" without usage, and never the brief's task.
func TestRunningRowHasNoTask(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now()
	rec := agent.ClaimRecord("claude", agent.RunOptions{Prompt: "Owned: [\"a.go\"]\nTask:\nfix the bug", Lead: "qa"}, now.Add(-time.Minute))
	rec.Status, rec.PID, rec.Model = "running", os.Getpid(), "databricks-claude-sonnet-5-5"
	if err := os.WriteFile(rec.EventsPath(), []byte(`{"type":"tool_use"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	renderTree(&b, &Tree{Dispatches: []Dispatch{dispatchFromRecord(rec, "", now)}}, now, 80, "")
	row := strings.Split(b.String(), "\n")[1]
	want := []string{"●", "claude", "claude-sonnet-5-5", "qa", "0s", "1m"}
	if got := strings.Fields(row); !slices.Equal(got, want) {
		t.Errorf("row = %q, want fields %q", row, want)
	}
}

// TestReapRepeatedDryRunStable: the stat-keyed settled skip must not change what a second Reap reports.
func TestReapRepeatedDryRunStable(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	orig := listProcs
	listProcs = func(string) ([]proc.Info, error) { return nil, nil }
	t.Cleanup(func() { listProcs = orig })
	writeBenchDispatchLog(t, 40, 5)
	var first, second strings.Builder
	if code := Reap(true, &first, io.Discard); code != 0 {
		t.Fatalf("first Reap = %d", code)
	}
	if code := Reap(true, &second, io.Discard); code != 0 {
		t.Fatalf("second Reap = %d", code)
	}
	if first.String() != second.String() {
		t.Fatalf("repeat differs:\n%s\nvs\n%s", first.String(), second.String())
	}
}

func TestReapReconcilesActiveIndex(t *testing.T) {
	reapSocketConfig(t)
	live := saveReapRecord(t, os.Getpid(), time.Now())
	dir := paths.DispatchDir()
	active := filepath.Join(dir, "active")
	_ = os.Remove(filepath.Join(active, live.ID+".json"))
	ghost := filepath.Join(active, "20261127T000000.000-ghost.json")
	if err := os.MkdirAll(active, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ghost, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := Reap(false, &out, &out); code != 0 {
		t.Fatalf("Reap = %d: %s", code, out.String())
	}
	if _, err := os.Stat(filepath.Join(active, live.ID+".json")); err != nil {
		t.Fatalf("Reap did not restore live active marker: %v", err)
	}
	if _, err := os.Stat(ghost); !os.IsNotExist(err) {
		t.Fatalf("Reap did not remove ghost marker: %v", err)
	}
}

func TestReapSilentStampCooldown(t *testing.T) {
	reapSocketConfig(t)
	oldCool := reapCooldown
	reapCooldown = 30 * time.Second
	t.Cleanup(func() { reapCooldown = oldCool })
	if code := Reap(false, io.Discard, io.Discard); code != 0 {
		t.Fatalf("initial silent Reap = %d", code)
	}
	sock := filepath.Join(paths.DispatchDir(), "orphan.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := Reap(false, io.Discard, io.Discard); code != 0 {
		t.Fatalf("throttled silent Reap = %d", code)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("throttled silent Reap removed socket during cooldown: %v", err)
	}
	past := time.Now().Add(-time.Minute)
	stamp := filepath.Join(paths.DispatchDir(), reapStampFile)
	if err := os.Chtimes(stamp, past, past); err != nil {
		t.Fatal(err)
	}
	if code := Reap(false, io.Discard, io.Discard); code != 0 {
		t.Fatalf("expired-stamp silent Reap = %d", code)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("silent Reap after cooldown did not remove socket: %v", err)
	}
}
