package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// sleeperScript writes a shell script under t.TempDir() that ignores its args and sleeps 30s.
func sleeperScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sleeper.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 30 &\nwait\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveExactID(t *testing.T) {
	recs := []Record{{ID: "20261016T210000.000-codex"}, {ID: "20261016T210100.000-codex"}}
	got, err := ResolveRecord(recs, "20261016T210100.000-codex", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "20261016T210100.000-codex" {
		t.Fatalf("got %q", got.ID)
	}
}

func TestResolveUniquePrefix(t *testing.T) {
	recs := []Record{{ID: "20261016T210000.000-codex"}, {ID: "20261016T210100.000-gemini"}}
	got, err := ResolveRecord(recs, "20261016T210100", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "20261016T210100.000-gemini" {
		t.Fatalf("got %q", got.ID)
	}
}

func TestResolveAmbiguousPrefixErrors(t *testing.T) {
	recs := []Record{{ID: "20261016T210000.000-codex"}, {ID: "20261016T210000.001-codex"}}
	if _, err := ResolveRecord(recs, "20261016T2100", nil); err == nil {
		t.Error("ambiguous prefix must error")
	}
}

func TestResolveEmptyRefPicksNewestMatchingPred(t *testing.T) {
	recs := []Record{
		{ID: "a", Status: "ok"},
		{ID: "b", Status: "running"},
		{ID: "c", Status: "ok"},
	}
	pred := func(r Record) bool { return r.Status == "running" }
	got, err := ResolveRecord(recs, "", pred)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "b" {
		t.Fatalf("got %q, want b (newest matching pred, recs assumed newest-first per RecentRecords)", got.ID)
	}
}

func TestResolveNoMatchErrors(t *testing.T) {
	recs := []Record{{ID: "a", Status: "ok"}}
	if _, err := ResolveRecord(recs, "zzz", nil); err == nil {
		t.Error("no matching id must error")
	}
}

func TestListFiltersByAgentAndRepo(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	base := time.Date(2026, 10, 16, 21, 0, 0, 0, time.UTC)
	mk := func(a, repo string, offset time.Duration) {
		r := NewRecord(a, RunOptions{Prompt: "p"}, base.Add(offset))
		r.Repo = repo
		if err := r.Save(); err != nil {
			t.Fatal(err)
		}
	}
	mk("codex", "/repoA", 0)
	mk("codex", "/repoB", time.Minute)
	mk("gemini", "/repoA", 2*time.Minute)

	got := List("codex", "/repoA", false)
	if len(got) != 1 || got[0].Repo != "/repoA" {
		t.Fatalf("List(codex, /repoA, false) = %+v, want 1 record from /repoA", got)
	}

	gotAll := List("codex", "/repoA", true)
	if len(gotAll) != 2 {
		t.Fatalf("List(codex, /repoA, true) = %d records, want 2 (repo filter lifted)", len(gotAll))
	}

	gotOther := List("gemini", "/repoA", false)
	if len(gotOther) != 1 {
		t.Fatalf("List(gemini, /repoA, false) = %d records, want 1", len(gotOther))
	}
}

func TestSpawnSetsPIDAndLogThenCancelStops(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	script := sleeperScript(t)
	old := executable
	executable = func() (string, error) { return script, nil }
	defer func() { executable = old }()

	rec := NewRecord("codex", RunOptions{Prompt: "p"}, time.Now())
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	if err := Spawn(&rec, nil); err != nil {
		t.Fatalf("Spawn() = %v", err)
	}
	if rec.PID <= 0 {
		t.Fatalf("rec.PID = %d, want > 0", rec.PID)
	}
	assertSpawnLog(t, rec.Log)
	if !rec.Alive() {
		t.Fatal("rec.Alive() = false right after Spawn, want true")
	}

	if err := Cancel(&rec); err != nil {
		t.Fatalf("Cancel() = %v", err)
	}
	if rec.Status != "error" || rec.Error == "" {
		t.Fatalf("after Cancel: status=%q error=%q, want error/cancelled", rec.Status, rec.Error)
	}
	if rec.Alive() {
		t.Error("rec.Alive() = true after Cancel, want false")
	}

	assertSavedCancellation(t, rec.ID)
}

// assertSavedCancellation checks the saved record reflects the cancellation too.
func assertSavedCancellation(t *testing.T, id string) {
	t.Helper()
	saved, err := LoadRecord(id)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != "error" {
		t.Fatalf("saved record status = %q, want error", saved.Status)
	}
}

func assertSpawnLog(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("log file must exist: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("log mode = %v, want 0600", fi.Mode().Perm())
	}
}

// A fast child that saves its own "ok" record immediately must not have that outcome overwritten:
// Spawn only sets rec.PID/Log in memory for the caller's print and must never save after cmd.Start().
func TestSpawnDoesNotSaveAfterFastChildFinishes(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := NewRecord("codex", RunOptions{Prompt: "p"}, time.Now())
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(paths.DispatchDir(), rec.ID+".json")

	// the fast child stands in for `claude-code agents ask --job <id>`: it saves its own finished record
	// (by writing the record file directly) and exits almost immediately.
	script := filepath.Join(t.TempDir(), "fast.sh")
	body := "#!/bin/sh\ncat > \"" + recordPath + "\" <<'EOF'\n" +
		`{"id":"` + rec.ID + `","status":"ok","agent":"codex"}` + "\nEOF\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	old := executable
	executable = func() (string, error) { return script, nil }
	defer func() { executable = old }()

	if err := Spawn(&rec, nil); err != nil {
		t.Fatalf("Spawn() = %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got, err := LoadRecord(rec.ID); err == nil && got.Status == "ok" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	got, err := LoadRecord(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "ok" {
		t.Fatalf("record on disk after fast child finished = %q, want %q (Spawn must not overwrite it)", got.Status, "ok")
	}
}

// Cancel must verify the PID still belongs to the job it was spawned as; a reused PID (here, a live
// unrelated process the test itself starts) must not be signalled, and the record is marked an error.
func TestCancelDetectsPIDReuse(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	unrelated := exec.Command("sleep", "30")
	if err := unrelated.Start(); err != nil {
		t.Fatalf("start unrelated process: %v", err)
	}
	t.Cleanup(func() { _ = unrelated.Process.Kill(); _ = unrelated.Wait() })

	rec := NewRecord("codex", RunOptions{Prompt: "p"}, time.Now())
	rec.PID = unrelated.Process.Pid                 // pretend the dispatch's old PID was reused by `sleep`
	rec.Log = filepath.Join(t.TempDir(), "job.log") // a background record: Log is set
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}

	err := Cancel(&rec)
	if err == nil || !strings.Contains(err.Error(), "different program (pid reused)") {
		t.Fatalf("Cancel() = %v, want the different-program error", err)
	}
	if rec.Status != "running" {
		t.Fatalf("status = %q: a reused pid must not rewrite the record", rec.Status)
	}
	if !proc.Alive(unrelated.Process.Pid) {
		t.Fatal("the unrelated live process must not have been signalled")
	}
}

// A child that traps and ignores SIGINT must still be ended by Cancel's SIGKILL escalation.
// t.Parallel rules out t.Setenv, which panics when mixed with parallel subtests; CLAUDE_CONFIG_DIR
// is saved/restored by hand instead. cancelGrace/killWait are shortened so escalation takes ~100ms.
func TestCancelEscalatesToSIGKILL(t *testing.T) {
	t.Parallel()
	oldGrace, oldKillWait := cancelGrace, killWait
	cancelGrace, killWait = 100*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { cancelGrace, killWait = oldGrace, oldKillWait })
	prevConfigDir, hadConfigDir := os.LookupEnv("CLAUDE_CONFIG_DIR")
	_ = os.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	defer func() {
		if hadConfigDir {
			_ = os.Setenv("CLAUDE_CONFIG_DIR", prevConfigDir)
		} else {
			_ = os.Unsetenv("CLAUDE_CONFIG_DIR")
		}
	}()
	script := filepath.Join(t.TempDir(), "trap.sh")
	// `sleep 30 &` + `wait` (not a tail-call `sleep 30`) keeps this shell's own pid alive and
	// trapping SIGINT instead of POSIX sh exec-optimizing the last command away.
	body := "#!/bin/sh\ntrap '' INT\necho ready\nsleep 30 &\nwait\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	old := executable
	executable = func() (string, error) { return script, nil }
	defer func() { executable = old }()

	rec := NewRecord("codex", RunOptions{Prompt: "p"}, time.Now())
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	if err := Spawn(&rec, nil); err != nil {
		t.Fatalf("Spawn() = %v", err)
	}
	if !rec.Alive() {
		t.Fatal("rec.Alive() = false right after Spawn, want true")
	}
	waitForTrapReady(t, rec.Log)

	if err := Cancel(&rec); err != nil {
		t.Fatalf("Cancel() = %v, want nil (SIGKILL escalation must end a SIGINT-trapping child)", err)
	}
	if rec.Status != "error" || rec.Error == "" {
		t.Fatalf("after Cancel: status=%q error=%q, want error/cancelled", rec.Status, rec.Error)
	}
	if rec.Alive() {
		t.Error("rec.Alive() = true after Cancel, want false (SIGKILL must have ended the trapping child)")
	}
}

// waitForTrapReady waits until the SIGINT trap is installed, so cancellation cannot beat it.
func waitForTrapReady(t *testing.T, log string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if b, _ := os.ReadFile(log); strings.Contains(string(b), "ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child never printed ready")
		}
	}
}

func TestCancelDeadPIDReturnsNilAndMarksRecord(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	rec := NewRecord("codex", RunOptions{Prompt: "p"}, time.Now())
	rec.PID = cmd.Process.Pid
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	if err := Cancel(&rec); err != nil {
		t.Fatalf("Cancel on a dead pid = %v, want nil", err)
	}
	if rec.Status != "error" {
		t.Errorf("status = %q, want error", rec.Status)
	}
}

// A foreground record (Log=="") whose pid is live but not `ask --job` is never signalled or rewritten.
func TestCancelForegroundRunErrorsWithoutWriting(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := NewRecord("codex", RunOptions{Prompt: "p"}, time.Now())
	rec.PID = os.Getpid() // the test process: alive, not a job
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(paths.DispatchDir(), rec.ID+".json"))
	err := Cancel(&rec)
	if err == nil || !strings.Contains(err.Error(), "foreground run") {
		t.Fatalf("Cancel() = %v, want the foreground-run error", err)
	}
	after, _ := os.ReadFile(filepath.Join(paths.DispatchDir(), rec.ID+".json"))
	if string(before) != string(after) {
		t.Errorf("record rewritten:\n%s\n->\n%s", before, after)
	}
	loaded, _ := LoadRecord(rec.ID)
	if loaded.Status != "running" || loaded.Error != "" {
		t.Errorf("loaded = %q/%q, want untouched running", loaded.Status, loaded.Error)
	}
}

// A dead pid whose record already finished is left untouched; a still-running one is marked once.
func TestCancelDeadPIDLeavesFinishedRecord(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	rec := NewRecord("codex", RunOptions{Prompt: "p"}, time.Now())
	rec.PID, rec.Status = cmd.Process.Pid, "ok"
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	stale := rec
	stale.Status = "running" // caller's copy is stale; disk says ok
	if err := Cancel(&stale); err != nil {
		t.Fatalf("Cancel = %v", err)
	}
	loaded, _ := LoadRecord(rec.ID)
	if loaded.Status != "ok" || loaded.Error != "" {
		t.Errorf("finished record changed to %q/%q", loaded.Status, loaded.Error)
	}
}

func TestCancelDeadPIDMarksRunningRecordOnce(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	rec := NewRecord("codex", RunOptions{Prompt: "p"}, time.Now())
	rec.PID = cmd.Process.Pid
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	if err := Cancel(&rec); err != nil {
		t.Fatal(err)
	}
	loaded, _ := LoadRecord(rec.ID)
	if loaded.Status != "error" || loaded.Error != "process gone" {
		t.Fatalf("loaded = %q/%q", loaded.Status, loaded.Error)
	}
	loaded.Error = "sentinel"
	if err := loaded.Save(); err != nil {
		t.Fatal(err)
	}
	if err := Cancel(&rec); err != nil {
		t.Fatal(err)
	}
	again, _ := LoadRecord(rec.ID)
	if again.Error != "sentinel" {
		t.Errorf("second Cancel rewrote the record: %q", again.Error)
	}
}

func TestIsJobArgvRequiresWholeArgs(t *testing.T) {
	id := "20261024T000000.000-codex"
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{"exact", []string{"/bin/claude-code", "agents", "ask", "--job", id}, true},
		{"pre-nesting argv", []string{"/bin/claude-code", "ask", "--job", id}, true},
		{"id substring only", []string{"claude-code", "ask", "--job", id + "-x"}, false},
		{"joined arg", []string{"claude-code", "ask --job " + id}, false},
		{"missing job flag", []string{"claude-code", "ask", id}, false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		if got := isJobArgv(c.argv, id); got != c.want {
			t.Errorf("%s: isJobArgv = %v, want %v", c.name, got, c.want)
		}
	}
}

// fixtureRecord saves rec under a fresh CLAUDE_CONFIG_DIR scoped to t, returning the saved record.
func fixtureRecord(t *testing.T, rec Record) Record {
	t.Helper()
	if err := rec.Save(); err != nil {
		t.Fatalf("Save(%+v): %v", rec, err)
	}
	return rec
}

// adapterDeadPID starts and waits out a child process, so its pid is guaranteed not to be alive.
func adapterDeadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn+wait a short-lived process: %v", err)
	}
	return cmd.Process.Pid
}

func setConfigDir(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
}

// TestStatusUnknownVendorErrors: Status rejects an unregistered vendor name before touching the dispatch log.
func TestStatusUnknownVendorErrors(t *testing.T) {
	setConfigDir(t)
	if _, err := Status(context.Background(), "nonexistent-vendor", t.TempDir(), "", false); err == nil {
		t.Error("Status(unknown agent) = nil error, want errUnknownAgent")
	}
}

// TestStatusRunningAlive: a running record whose pid is this test process reports Status "running".
func TestStatusRunningAlive(t *testing.T) {
	setConfigDir(t)
	cwd := t.TempDir()
	base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	rec := NewRecord("codex", RunOptions{Prompt: "do the thing"}, base)
	rec.Repo, rec.PID = cwd, os.Getpid()
	fixtureRecord(t, rec)

	rows, err := Status(context.Background(), "codex", cwd, "", false)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("Status rows = %v, want 1", rows)
	}
	if rows[0].Status != "running" {
		t.Errorf("rows[0].Status = %q, want running (pid %d is this test process)", rows[0].Status, os.Getpid())
	}
	if rows[0].Preview != "do the thing" {
		t.Errorf("rows[0].Preview = %q, want %q", rows[0].Preview, "do the thing")
	}
}

// TestStatusRunningLostDeadPID: a record still marked running but whose pid has exited reports "lost",
// not "running" — this is the teeth case: rec.Alive() must actually gate the row, not just echo r.Status.
func TestStatusRunningLostDeadPID(t *testing.T) {
	setConfigDir(t)
	cwd := t.TempDir()
	base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	rec := NewRecord("codex", RunOptions{Prompt: "orphaned"}, base)
	rec.Repo, rec.PID = cwd, adapterDeadPID(t)
	fixtureRecord(t, rec)

	rows, err := Status(context.Background(), "codex", cwd, "", false)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != "lost" {
		t.Fatalf("Status rows = %v, want one row with Status lost", rows)
	}
}

// TestResultRunningErrors: a still-running record, fetched by its exact id, is refused with "still running" —
// JobResult must not hand back a log for a job that has not finished.
func TestResultRunningErrors(t *testing.T) {
	setConfigDir(t)
	cwd := t.TempDir()
	base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	rec := NewRecord("codex", RunOptions{Prompt: "still going"}, base)
	rec.Repo, rec.PID = cwd, os.Getpid()
	fixtureRecord(t, rec)

	_, _, err := JobResult(context.Background(), "codex", cwd, rec.ID)
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Errorf("Result(running job) err = %v, want it to mention \"still running\"", err)
	}
}

// TestResultOK: a finished record returns its record and log text.
func TestResultOK(t *testing.T) {
	setConfigDir(t)
	cwd := t.TempDir()
	logPath := filepath.Join(cwd, "job.log")
	if err := os.WriteFile(logPath, []byte("the answer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	rec := NewRecord("codex", RunOptions{Prompt: "finished job"}, base)
	rec.Repo, rec.Status, rec.Log, rec.SessionID = cwd, "ok", logPath, "sess-1"
	fixtureRecord(t, rec)

	got, log, err := JobResult(context.Background(), "codex", cwd, rec.ID)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if got.ID != rec.ID || got.SessionID != "sess-1" {
		t.Errorf("Result record = %+v, want ID %q SessionID sess-1", got, rec.ID)
	}
	if log != "the answer\n" {
		t.Errorf("Result log = %q, want %q", log, "the answer\n")
	}
}

// TestCancelJobNoneRunningErrors: with no running job for this vendor/repo, CancelJob errors rather
// than cancelling an unrelated finished record.
func TestCancelJobNoneRunningErrors(t *testing.T) {
	setConfigDir(t)
	cwd := t.TempDir()
	base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	rec := NewRecord("codex", RunOptions{Prompt: "already done"}, base)
	rec.Repo, rec.Status = cwd, "ok"
	fixtureRecord(t, rec)

	if _, err := CancelJob(context.Background(), "codex", cwd, ""); err == nil {
		t.Error("CancelJob with zero running jobs = nil error, want one")
	}
}

// TestCancelJobAmbiguousPrefixErrors: a ref prefix matching several dispatches is refused rather than
// guessing which one to cancel.
func TestCancelJobAmbiguousPrefixErrors(t *testing.T) {
	setConfigDir(t)
	cwd := t.TempDir()
	base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, suffix := range []string{"-a", "-b"} {
		rec := NewRecord("codex", RunOptions{Prompt: "dup"}, base)
		rec.ID = "20261024T120000.000-codex" + suffix
		rec.Repo, rec.PID = cwd, os.Getpid()
		fixtureRecord(t, rec)
	}

	_, err := CancelJob(context.Background(), "codex", cwd, "20261024T120000")
	if err == nil || !strings.Contains(err.Error(), "matches 2 dispatches") {
		t.Errorf("CancelJob(ambiguous prefix) err = %v, want \"matches 2 dispatches\"", err)
	}
}

// TestCancelJobUnknownVendorErrors: CancelJob rejects an unregistered vendor before touching the log.
func TestCancelJobUnknownVendorErrors(t *testing.T) {
	setConfigDir(t)
	if _, err := CancelJob(context.Background(), "nonexistent-vendor", t.TempDir(), ""); err == nil {
		t.Error("CancelJob(unknown agent) = nil error, want errUnknownAgent")
	}
}

// TestFindAmbiguousPrefix: a prefix shared by two dispatches errors and names the count; an exact id resolves.
func TestFindAmbiguousPrefix(t *testing.T) {
	setConfigDir(t)
	a := fixtureRecord(t, NewRecord("claude", RunOptions{Prompt: "a"}, time.Now().Add(-2*time.Hour)))
	b := fixtureRecord(t, NewRecord("codex", RunOptions{Prompt: "b"}, time.Now().Add(-time.Hour)))
	prefix := a.ID[:4]
	if _, err := Find(prefix); err == nil || !strings.Contains(err.Error(), "2 matches") {
		t.Errorf("Find(%q) err = %v, want an ambiguity error", prefix, err)
	}
	got, err := Find(b.ID)
	if err != nil || got.Agent != "codex" {
		t.Errorf("Find(exact) = %+v, %v, want the codex record", got, err)
	}
}

func TestCancelRejectsReusedSplitReviewPID(t *testing.T) {
	for _, identity := range []string{"stale", "missing"} {
		t.Run(identity, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			old := executable
			executable = func() (string, error) { return sleeperScript(t), nil }
			t.Cleanup(func() { executable = old })
			rec := ClaimRecord("codex", RunOptions{}, time.Now())
			rec.Progress = "part 1/2"
			if err := SpawnArgv(&rec, []string{"codex", "review", "--background"}, nil); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = kill(rec.PID) })
			if !sameStart(&rec) {
				t.Fatal("spawned review has no matching recorded process identity")
			}
			rec.PIDStart += "-stale"
			if identity == "missing" {
				rec.PIDStart = ""
			}
			if err := rec.Save(); err != nil {
				t.Fatal(err)
			}
			err := Cancel(&rec)
			if err == nil || !strings.Contains(err.Error(), "not the process spawned for it (pid reused); not signalling it") {
				t.Errorf("Cancel() = %v, want process-identity refusal", err)
			}
			assertReviewUnaffected(t, rec)
		})
	}
}

func assertReviewUnaffected(t *testing.T, rec Record) {
	t.Helper()
	if !rec.Alive() {
		t.Error("Cancel signalled an unrelated split review with matching command arguments")
	}
	saved, err := LoadRecord(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != "running" || saved.Error != "" {
		t.Errorf("Cancel rewrote unrelated review record: status=%q error=%q", saved.Status, saved.Error)
	}
}

// skipOffUnix skips tests that need real POSIX process groups and signals.
func skipOffUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("process-group signals need unix")
	}
}

// A leader in its own process group runs a second process in the foreground (same group, different pid) that traps
// SIGINT; the marker proves interrupt(leaderPid) reaches the whole group, not just the leader pid passed in. The
// leader's INT trap is a no-op handler, not ” (ignore): an ignored signal is inherited across exec, a handler is not.
func TestInterruptReachesWholeProcessGroup(t *testing.T) {
	skipOffUnix(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "child-got-sigint")
	childScript := filepath.Join(dir, "child.sh")
	leaderScript := filepath.Join(dir, "leader.sh")

	childBody := "#!/bin/sh\ntrap 'touch \"" + marker + "\"; exit 0' INT\nsleep 30 &\nwait\n"
	if err := os.WriteFile(childScript, []byte(childBody), 0o700); err != nil {
		t.Fatal(err)
	}
	leaderBody := "#!/bin/sh\ntrap ':' INT\n\"" + childScript + "\"\n"
	if err := os.WriteFile(leaderScript, []byte(leaderBody), 0o700); err != nil {
		t.Fatal(err)
	}

	leader := exec.CommandContext(context.Background(), leaderScript)
	proc.OwnGroup(leader)
	if err := leader.Start(); err != nil {
		t.Fatalf("start leader: %v", err)
	}
	leaderPid := leader.Process.Pid
	t.Cleanup(func() {
		_ = kill(leaderPid)
		_ = leader.Wait()
	})

	// give both processes time to install their traps before signalling, else SIGINT can race trap installation.
	time.Sleep(200 * time.Millisecond)

	if err := interrupt(leaderPid); err != nil {
		t.Fatalf("interrupt(leaderPid) = %v, want nil", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return // the child, a different pid in the same group, observed the SIGINT.
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("child never observed SIGINT: interrupt(leaderPid) did not reach the whole process group")
}

// A pid that has already exited (ESRCH) is success, not an error — the caller never needs to special-case it.
func TestInterruptOnExitedPIDReturnsNil(t *testing.T) {
	skipOffUnix(t)
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := interrupt(cmd.Process.Pid); err != nil {
		t.Fatalf("interrupt(exited pid) = %v, want nil (ESRCH is success)", err)
	}
}

func TestSpawnArgvScrubsInheritedSecrets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HARNESS_KEEP_ENV", "")
	t.Setenv("ANTHROPIC_API_KEY", "fake-inherited-canary")
	t.Setenv("HARNESS_VAULT_KEY", "fake-vault-key")
	script := filepath.Join(dir, "child")
	body := "#!/bin/sh\nprintf '%s\\n' \"$REGRESSION_EXTRA\" \"${ANTHROPIC_API_KEY+set}\" \"$HARNESS_VAULT_KEY\" done\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	old := executable
	executable = func() (string, error) { return script, nil }
	t.Cleanup(func() { executable = old })
	rec := Record{Log: filepath.Join(dir, "child.log")}
	if err := SpawnArgv(&rec, nil, []string{"REGRESSION_EXTRA=explicit-extra"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		got, err := os.ReadFile(rec.Log)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(string(got), "done\n") {
			if string(got) != "explicit-extra\n\nfake-vault-key\ndone\n" {
				t.Fatalf("job child environment = %q; want Extra, secret unset, and Claude vault key retained", got)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("job child did not finish writing its environment")
		case <-tick.C:
		}
	}
}

func TestCancelDetachedSplitReview(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	old := executable
	executable = func() (string, error) { return sleeperScript(t), nil }
	t.Cleanup(func() { executable = old })
	rec := ClaimRecord("codex", RunOptions{}, time.Now())
	rec.Progress = "part 1/2"
	if err := SpawnArgv(&rec, []string{"codex", "review", "--background"}, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kill(rec.PID) })
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := CancelJob(context.Background(), "codex", "", rec.ID)
	if err != nil {
		t.Fatalf("cancel detached split review: %v", err)
	}
	if got.Alive() || got.Status != "error" || !strings.Contains(got.Error, "cancelled") {
		t.Fatalf("cancelled review = %+v, want stopped and recorded cancellation", got)
	}
	assertSavedCancellation(t, rec.ID)
}
