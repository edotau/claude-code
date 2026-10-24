package vendoragent

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/proc"
)

// sleeperScript writes a shell script under t.TempDir() that ignores its args and sleeps 30s.
func sleeperScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sleeper.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveExactID(t *testing.T) {
	recs := []agent.Record{{ID: "20261016T210000.000-codex"}, {ID: "20261016T210100.000-codex"}}
	got, err := Resolve(recs, "20261016T210100.000-codex", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "20261016T210100.000-codex" {
		t.Fatalf("got %q", got.ID)
	}
}

func TestResolveUniquePrefix(t *testing.T) {
	recs := []agent.Record{{ID: "20261016T210000.000-codex"}, {ID: "20261016T210100.000-gemini"}}
	got, err := Resolve(recs, "20261016T210100", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "20261016T210100.000-gemini" {
		t.Fatalf("got %q", got.ID)
	}
}

func TestResolveAmbiguousPrefixErrors(t *testing.T) {
	recs := []agent.Record{{ID: "20261016T210000.000-codex"}, {ID: "20261016T210000.001-codex"}}
	if _, err := Resolve(recs, "20261016T2100", nil); err == nil {
		t.Error("ambiguous prefix must error")
	}
}

func TestResolveEmptyRefPicksNewestMatchingPred(t *testing.T) {
	recs := []agent.Record{
		{ID: "a", Status: "ok"},
		{ID: "b", Status: "running"},
		{ID: "c", Status: "ok"},
	}
	pred := func(r agent.Record) bool { return r.Status == "running" }
	got, err := Resolve(recs, "", pred)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "b" {
		t.Fatalf("got %q, want b (newest matching pred, recs assumed newest-first per RecentRecords)", got.ID)
	}
}

func TestResolveNoMatchErrors(t *testing.T) {
	recs := []agent.Record{{ID: "a", Status: "ok"}}
	if _, err := Resolve(recs, "zzz", nil); err == nil {
		t.Error("no matching id must error")
	}
}

func TestListFiltersByAgentAndRepo(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	base := time.Date(2026, 10, 16, 21, 0, 0, 0, time.UTC)
	mk := func(a, repo string, offset time.Duration) {
		r := agent.NewRecord(a, agent.Request{Prompt: "p"}, base.Add(offset))
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

	rec := agent.NewRecord("codex", agent.Request{Prompt: "p"}, time.Now())
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	if err := Spawn(&rec, nil); err != nil {
		t.Fatalf("Spawn() = %v", err)
	}
	if rec.PID <= 0 {
		t.Fatalf("rec.PID = %d, want > 0", rec.PID)
	}
	fi, err := os.Stat(rec.Log)
	if err != nil {
		t.Fatalf("log file must exist: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("log mode = %v, want 0600", fi.Mode().Perm())
	}
	if !Alive(rec) {
		t.Fatal("Alive(rec) = false right after Spawn, want true")
	}

	if err := Cancel(&rec); err != nil {
		t.Fatalf("Cancel() = %v", err)
	}
	if rec.Status != "error" || rec.Error == "" {
		t.Fatalf("after Cancel: status=%q error=%q, want error/cancelled", rec.Status, rec.Error)
	}
	if Alive(rec) {
		t.Error("Alive(rec) = true after Cancel, want false")
	}

	// the saved record on disk must reflect the cancellation too.
	saved, err := agent.LoadRecord(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != "error" {
		t.Fatalf("saved record status = %q, want error", saved.Status)
	}
}

// A fast child that saves its own "ok" record immediately must not have that outcome overwritten:
// Spawn only sets rec.PID/Log in memory for the caller's print and must never save after cmd.Start().
func TestSpawnDoesNotSaveAfterFastChildFinishes(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := agent.NewRecord("codex", agent.Request{Prompt: "p"}, time.Now())
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(agent.DispatchDir(), rec.ID+".json")

	// the fast child stands in for `claude-code ask --job <id>`: it saves its own finished record
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
		if got, err := agent.LoadRecord(rec.ID); err == nil && got.Status == "ok" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	got, err := agent.LoadRecord(rec.ID)
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

	rec := agent.NewRecord("codex", agent.Request{Prompt: "p"}, time.Now())
	rec.PID = unrelated.Process.Pid // pretend the dispatch's old PID was reused by `sleep`
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}

	if err := Cancel(&rec); err != nil {
		t.Fatalf("Cancel() = %v, want nil (pid-reuse is reported via the record, not an error)", err)
	}
	if rec.Status != "error" || rec.Error != "process gone (pid reused)" {
		t.Fatalf("after Cancel on reused pid: status=%q error=%q, want error/%q", rec.Status, rec.Error, "process gone (pid reused)")
	}
	if !proc.Alive(unrelated.Process.Pid) {
		t.Fatal("the unrelated live process must not have been signalled")
	}
}

// A child that traps and ignores SIGINT must still be ended by Cancel's SIGKILL escalation.
// t.Parallel (this test takes ~5s waiting out the SIGINT grace period) rules out t.Setenv, which
// panics when mixed with parallel subtests; CLAUDE_CONFIG_DIR is saved/restored by hand instead.
func TestCancelEscalatesToSIGKILL(t *testing.T) {
	t.Parallel()
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
	body := "#!/bin/sh\ntrap '' INT\nsleep 30 &\nwait\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	old := executable
	executable = func() (string, error) { return script, nil }
	defer func() { executable = old }()

	rec := agent.NewRecord("codex", agent.Request{Prompt: "p"}, time.Now())
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	if err := Spawn(&rec, nil); err != nil {
		t.Fatalf("Spawn() = %v", err)
	}
	if !Alive(rec) {
		t.Fatal("Alive(rec) = false right after Spawn, want true")
	}
	// give the shell time to run `trap '' INT` before signalling it, else SIGINT can race the
	// trap installation and kill it on the default disposition instead of exercising escalation.
	time.Sleep(200 * time.Millisecond)

	if err := Cancel(&rec); err != nil {
		t.Fatalf("Cancel() = %v, want nil (SIGKILL escalation must end a SIGINT-trapping child)", err)
	}
	if rec.Status != "error" || rec.Error == "" {
		t.Fatalf("after Cancel: status=%q error=%q, want error/cancelled", rec.Status, rec.Error)
	}
	if Alive(rec) {
		t.Error("Alive(rec) = true after Cancel, want false (SIGKILL must have ended the trapping child)")
	}
}
