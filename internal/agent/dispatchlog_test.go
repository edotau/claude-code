package agent

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A record saves as running, finishes with its outcome, loads by prefix, and the log prunes the oldest.
func TestDispatchRecordLifecycleAndPrune(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	base := time.Date(2026, 10, 16, 21, 0, 0, 0, time.UTC)
	r := NewRecord("codex", Request{Prompt: "p", Tools: true}, base)
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadRecord("20261016T210000"); got.Status != "running" || got.Request.Prompt != "p" || !got.Request.Tools {
		t.Fatalf("running record = %+v", got)
	}
	r.Finish(Result{Provider: "chatgpt"}, nil, time.Second)
	_ = r.Save()
	if got, _ := LoadRecord(r.ID); got.Status != "ok" || got.Provider != "chatgpt" {
		t.Fatalf("finished record = %+v", got)
	}
	if fi, _ := os.Stat(filepath.Join(DispatchDir(), r.ID+".json")); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	old := dispatchKeep
	dispatchKeep = 2
	defer func() { dispatchKeep = old }()
	for i := 1; i <= 3; i++ {
		_ = NewRecord("gemini", Request{Prompt: "q"}, base.Add(time.Duration(i)*time.Minute)).Save()
	}
	if recs := RecentRecords(10); len(recs) != 2 || recs[0].Time.Before(recs[1].Time) {
		t.Fatalf("after prune: %d records, newest first = %v", len(recs), recs)
	}
	if _, err := LoadRecord("2026"); err == nil {
		t.Error("an ambiguous prefix must not load")
	}
}

// Repo/PID/Log round-trip through JSON (and are omitted when zero).
func TestRecordJSONRoundTripsRepoPIDLog(t *testing.T) {
	r := Record{ID: "x", Agent: "codex", Repo: "/repo", PID: 4242, Log: "/repo/x.log"}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); !strings.Contains(s, `"repo":"/repo"`) || !strings.Contains(s, `"pid":4242`) || !strings.Contains(s, `"log":"/repo/x.log"`) {
		t.Fatalf("marshaled record missing repo/pid/log: %s", s)
	}
	var got Record
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Repo != r.Repo || got.PID != r.PID || got.Log != r.Log {
		t.Fatalf("round trip = %+v, want %+v", got, r)
	}

	zero := Record{ID: "y"}
	zb, err := json.Marshal(zero)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(zb); strings.Contains(s, `"repo"`) || strings.Contains(s, `"pid"`) || strings.Contains(s, `"log"`) {
		t.Fatalf("zero-valued repo/pid/log must be omitted: %s", s)
	}
}

// pruneDispatches removes the pruned record's .log alongside its .json.
func TestPruneDispatchesRemovesLog(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	base := time.Date(2026, 10, 16, 21, 0, 0, 0, time.UTC)
	old := dispatchKeep
	dispatchKeep = 1
	defer func() { dispatchKeep = old }()

	first := NewRecord("codex", Request{Prompt: "p"}, base)
	if err := first.Save(); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(DispatchDir(), first.ID+".log")
	if err := os.WriteFile(logPath, []byte("job output"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("log must exist before prune: %v", err)
	}

	// second record pushes the first past dispatchKeep=1, triggering prune
	if err := NewRecord("codex", Request{Prompt: "q"}, base.Add(time.Minute)).Save(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("pruned record's log must be removed, stat err = %v", err)
	}
}

// An active background job (Status running, Log set, PID alive) survives the prune past dispatchKeep
// while older finished records are removed; a background job with a dead PID is pruned; a foreground
// running record (Log=="") is pruned even though it is still "running".
func TestPruneDispatchesSkipsActiveBackgroundRecord(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	base := time.Date(2026, 10, 16, 21, 0, 0, 0, time.UTC)
	old := dispatchKeep
	dispatchKeep = 2
	defer func() { dispatchKeep = old }()

	// a live process to stand in for the active job's PID.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	// active sits 2nd-oldest so the prune walk reaches it before excess is exhausted; if the active
	// guard were missing, the walk would remove it here instead of skipping on to trigger1.
	finished := NewRecord("codex", Request{Prompt: "old"}, base)
	finished.Status = "ok"
	if err := finished.Save(); err != nil {
		t.Fatal(err)
	}

	active := NewRecord("codex", Request{Prompt: "bg"}, base.Add(time.Minute))
	active.Log = filepath.Join(DispatchDir(), active.ID+".log")
	active.PID = cmd.Process.Pid
	if err := active.Save(); err != nil {
		t.Fatal(err)
	}

	trigger1 := NewRecord("codex", Request{Prompt: "newer"}, base.Add(2*time.Minute))
	trigger1.Status = "ok"
	if err := trigger1.Save(); err != nil {
		t.Fatal(err)
	}

	// 4th record with dispatchKeep=2 sets excess=2: without the guard, the walk would consume that
	// excess on `finished` then `active` and leave `trigger1` alone; proving teeth: it does not.
	trigger2 := NewRecord("codex", Request{Prompt: "newest"}, base.Add(3*time.Minute))
	trigger2.Status = "ok"
	if err := trigger2.Save(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(DispatchDir(), finished.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("finished record must be pruned, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(DispatchDir(), active.ID+".json")); err != nil {
		t.Fatalf("active background record must survive the prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(DispatchDir(), trigger1.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("trigger1 must be pruned in active's place, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(DispatchDir(), trigger2.ID+".json")); err != nil {
		t.Fatalf("newest record must survive the prune: %v", err)
	}
}

// A background running record whose PID is no longer alive is not "active" and gets pruned.
func TestPruneDispatchesPrunesDeadPIDBackgroundRecord(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	base := time.Date(2026, 10, 16, 21, 0, 0, 0, time.UTC)
	old := dispatchKeep
	dispatchKeep = 1
	defer func() { dispatchKeep = old }()

	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn a short-lived process: %v", err)
	}

	dead := NewRecord("codex", Request{Prompt: "bg"}, base)
	dead.Log = filepath.Join(DispatchDir(), dead.ID+".log")
	dead.PID = cmd.Process.Pid // reaped, no longer alive
	if err := dead.Save(); err != nil {
		t.Fatal(err)
	}

	if err := NewRecord("codex", Request{Prompt: "new"}, base.Add(time.Minute)).Save(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(DispatchDir(), dead.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("background record with a dead PID must be pruned, stat err = %v", err)
	}
}

// A foreground running record (Log=="") is pruned even while Status=="running": only a background
// job (Log set) is ever treated as active.
func TestPruneDispatchesPrunesForegroundRunningRecord(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	base := time.Date(2026, 10, 16, 21, 0, 0, 0, time.UTC)
	old := dispatchKeep
	dispatchKeep = 1
	defer func() { dispatchKeep = old }()

	fg := NewRecord("codex", Request{Prompt: "fg"}, base) // Status "running", Log left empty
	if err := fg.Save(); err != nil {
		t.Fatal(err)
	}

	if err := NewRecord("codex", Request{Prompt: "new"}, base.Add(time.Minute)).Save(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(DispatchDir(), fg.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("foreground running record (no Log) must be pruned, stat err = %v", err)
	}
}
