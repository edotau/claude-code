package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// A record saves as running, finishes with its outcome, loads by prefix, and the log prunes the oldest.
func TestDispatchRecordLifecycleAndPrune(t *testing.T) {
	setConfigDir(t)
	base := time.Date(2026, 10, 16, 21, 0, 0, 0, time.UTC)
	r := NewRecord("codex", RunOptions{Prompt: "p", Tools: true}, base)
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadRecord("20261016T210000"); got.Status != "running" || got.Request.Prompt != "p" || !got.Request.Tools {
		t.Fatalf("running record = %+v", got)
	}
	r.Finish(wire.Result{Provider: "chatgpt"}, nil, time.Second)
	_ = r.Save()
	if got, _ := LoadRecord(r.ID); got.Status != "ok" || got.Provider != "chatgpt" {
		t.Fatalf("finished record = %+v", got)
	}
	if fi, _ := os.Stat(filepath.Join(paths.DispatchDir(), r.ID+".json")); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	assertDispatchRecordPruning(t, base)
}

func assertDispatchRecordPruning(t *testing.T, base time.Time) {
	t.Helper()
	old := dispatchKeep
	dispatchKeep = 2
	defer func() { dispatchKeep = old }()
	for i := 1; i <= dispatchKeep+dispatchPruneMargin; i++ {
		if err := NewRecord("gemini", RunOptions{Prompt: "q"}, base.Add(time.Duration(i)*time.Minute)).Save(); err != nil {
			t.Fatal(err)
		}
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

	t.Run("zero fields omitted", assertRecordZeroFieldsOmitted)
}

func assertRecordZeroFieldsOmitted(t *testing.T) {
	t.Helper()
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
	setConfigDir(t)
	base := time.Date(2026, 10, 16, 21, 0, 0, 0, time.UTC)
	old := dispatchKeep
	dispatchKeep = 1
	defer func() { dispatchKeep = old }()

	first := NewRecord("codex", RunOptions{Prompt: "p"}, base)
	if err := first.Save(); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(paths.DispatchDir(), first.ID+".log")
	if err := os.WriteFile(logPath, []byte("job output"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("log must exist before prune: %v", err)
	}

	savePruneRecords(t, base.Add(time.Minute), dispatchKeep+dispatchPruneMargin)

	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("pruned record's log must be removed, stat err = %v", err)
	}
}

// An active background job (Status running, Log set, PID alive) survives the prune past dispatchKeep
// while older finished records are removed; a background job with a dead PID is pruned; a foreground
// running record (Log=="") is pruned even though it is still "running".
func TestPruneDispatchesSkipsActiveBackgroundRecord(t *testing.T) {
	setConfigDir(t)
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
	finished := NewRecord("codex", RunOptions{Prompt: "old"}, base)
	finished.Status = "ok"
	if err := finished.Save(); err != nil {
		t.Fatal(err)
	}

	active := NewRecord("codex", RunOptions{Prompt: "bg"}, base.Add(time.Minute))
	active.Log = filepath.Join(paths.DispatchDir(), active.ID+".log")
	active.PID = cmd.Process.Pid
	if err := active.Save(); err != nil {
		t.Fatal(err)
	}

	trigger1 := NewRecord("codex", RunOptions{Prompt: "newer"}, base.Add(2*time.Minute))
	trigger1.Status = "ok"
	if err := trigger1.Save(); err != nil {
		t.Fatal(err)
	}

	savePruneRecords(t, base.Add(3*time.Minute), dispatchPruneMargin-1)
	// Cross the margin so trigger1 must be pruned in active's place.
	trigger2 := NewRecord("codex", RunOptions{Prompt: "newest"}, base.Add(time.Duration(dispatchPruneMargin+2)*time.Minute))
	trigger2.Status = "ok"
	if err := trigger2.Save(); err != nil {
		t.Fatal(err)
	}

	assertBackgroundPruneRecords(t, finished, active, trigger1, trigger2)
}

func assertBackgroundPruneRecords(t *testing.T, finished, active, trigger1, trigger2 Record) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(paths.DispatchDir(), finished.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("finished record must be pruned, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths.DispatchDir(), active.ID+".json")); err != nil {
		t.Fatalf("active background record must survive the prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths.DispatchDir(), trigger1.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("trigger1 must be pruned in active's place, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths.DispatchDir(), trigger2.ID+".json")); err != nil {
		t.Fatalf("newest record must survive the prune: %v", err)
	}
}

// A background running record whose PID is no longer alive is not "active" and gets pruned.
func TestPruneDispatchesPrunesDeadPIDBackgroundRecord(t *testing.T) {
	setConfigDir(t)
	base := time.Date(2026, 10, 16, 21, 0, 0, 0, time.UTC)
	old := dispatchKeep
	dispatchKeep = 1
	defer func() { dispatchKeep = old }()

	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn a short-lived process: %v", err)
	}

	dead := NewRecord("codex", RunOptions{Prompt: "bg"}, base)
	dead.Log = filepath.Join(paths.DispatchDir(), dead.ID+".log")
	dead.PID = cmd.Process.Pid // reaped, no longer alive
	if err := dead.Save(); err != nil {
		t.Fatal(err)
	}

	savePruneRecords(t, base.Add(time.Minute), dispatchKeep+dispatchPruneMargin)

	if _, err := os.Stat(filepath.Join(paths.DispatchDir(), dead.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("background record with a dead PID must be pruned, stat err = %v", err)
	}
}

// A foreground running record (Log=="") is pruned even while Status=="running": only a background
// job (Log set) is ever treated as active.
func TestPruneDispatchesPrunesForegroundRunningRecord(t *testing.T) {
	setConfigDir(t)
	base := time.Date(2026, 10, 16, 21, 0, 0, 0, time.UTC)
	old := dispatchKeep
	dispatchKeep = 1
	defer func() { dispatchKeep = old }()

	fg := NewRecord("codex", RunOptions{Prompt: "fg"}, base) // Status "running", Log left empty
	if err := fg.Save(); err != nil {
		t.Fatal(err)
	}

	savePruneRecords(t, base.Add(time.Minute), dispatchKeep+dispatchPruneMargin)

	if _, err := os.Stat(filepath.Join(paths.DispatchDir(), fg.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("foreground running record (no Log) must be pruned, stat err = %v", err)
	}
}

// Record.Alive table test: status gates everything; PID>0 defers to proc.Alive; PID==0 falls back
// to a one-minute grace window on Time. Each case's assertion fails if the matching guard reverts
// (e.g. dropping the `r.Status != "running"` check breaks the "done" case; dropping the PID==0 time
// fallback breaks the "2 min ago" case).
func TestRecordAlive(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn+wait a short-lived process: %v", err)
	}
	deadPID := cmd.Process.Pid

	cases := []struct {
		name string
		r    Record
		want bool
	}{
		{"running, pid of this process", Record{Status: "running", PID: os.Getpid()}, true},
		{"running, dead pid", Record{Status: "running", PID: deadPID}, false},
		{"running, pid 0, time now", Record{Status: "running", PID: 0, Time: time.Now()}, true},
		{"running, pid 0, time 2 min ago", Record{Status: "running", PID: 0, Time: time.Now().Add(-2 * time.Minute)}, false},
		{"status done", Record{Status: "done", PID: os.Getpid()}, false},
		{"status ok, pid 0, time now", Record{Status: "ok", PID: 0, Time: time.Now()}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.r.Alive(); got != c.want {
				t.Errorf("Alive() = %v, want %v", got, c.want)
			}
		})
	}
}

// Prune keeps a record alive by the PID==0/recent-Time path (no PID at all) and drops a non-alive
// "running" record with PID==0 whose Time is stale — proving activeRecord routes through Alive()
// rather than only checking Log!="" or PID>0.
func TestPruneDispatchesKeepsAlivePID0RecordDropsStalePID0Record(t *testing.T) {
	setConfigDir(t)
	base := time.Date(2026, 10, 16, 21, 0, 0, 0, time.UTC)
	old := dispatchKeep
	dispatchKeep = 2
	defer func() { dispatchKeep = old }()

	stale := NewRecord("codex", RunOptions{Prompt: "stale"}, base) // PID 0, Time stays 8 days in the past: not alive
	if err := stale.Save(); err != nil {
		t.Fatal(err)
	}

	// alive's ID sorts as 2nd-oldest (so the prune walk reaches it before excess is exhausted), but
	// its on-disk Time is overridden to "just now" so Alive() reports it live despite Status "running".
	alive := NewRecord("codex", RunOptions{Prompt: "alive"}, base.Add(30*time.Second))
	alive.Time = time.Now().Add(-5 * time.Second)
	if err := alive.Save(); err != nil {
		t.Fatal(err)
	}

	trigger1 := NewRecord("codex", RunOptions{Prompt: "trigger1"}, base.Add(time.Minute))
	trigger1.Status = "ok"
	if err := trigger1.Save(); err != nil {
		t.Fatal(err)
	}
	savePruneRecords(t, base.Add(2*time.Minute), dispatchPruneMargin-1)
	trigger2 := NewRecord("codex", RunOptions{Prompt: "trigger2"}, base.Add(time.Duration(dispatchPruneMargin+1)*time.Minute))
	trigger2.Status = "ok"
	if err := trigger2.Save(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(paths.DispatchDir(), stale.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("stale PID==0 record must be pruned, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths.DispatchDir(), alive.ID+".json")); err != nil {
		t.Fatalf("recent PID==0 record must survive the prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths.DispatchDir(), trigger1.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("trigger1 must be pruned in alive's place, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths.DispatchDir(), trigger2.ID+".json")); err != nil {
		t.Fatalf("newest record must survive the prune: %v", err)
	}
}

func savePruneRecords(t *testing.T, base time.Time, count int) {
	t.Helper()
	for i := range count {
		rec := NewRecord("codex", RunOptions{Prompt: "filler"}, base.Add(time.Duration(i)*time.Minute))
		rec.Status = "ok"
		if err := rec.Save(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPruneDispatchesMarginBoundaryKeepsOldestActiveRecord(t *testing.T) {
	setConfigDir(t)
	old := dispatchKeep
	dispatchKeep = 2
	t.Cleanup(func() { dispatchKeep = old })
	base := time.Date(2026, 10, 16, 21, 0, 0, 0, time.UTC)
	active := NewRecord("codex", RunOptions{Prompt: "oldest active"}, base)
	active.PID = os.Getpid()
	if err := active.Save(); err != nil {
		t.Fatal(err)
	}
	dir := paths.DispatchDir()
	list := func() []string {
		names, err := recordNames(dir)
		if err != nil {
			t.Fatal(err)
		}
		return names
	}
	savePruneRecords(t, base.Add(time.Minute), dispatchKeep+dispatchPruneMargin-1)
	before := list()
	if len(before) != dispatchKeep+dispatchPruneMargin {
		t.Fatalf("at boundary: got %d records, want %d", len(before), dispatchKeep+dispatchPruneMargin)
	}
	if before[0] != active.ID+".json" {
		t.Fatalf("oldest record = %q, want active %q", before[0], active.ID)
	}
	pruneDispatches(dir)
	if after := list(); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("pruning at boundary changed records: before %v, after %v", before, after)
	}
	savePruneRecords(t, base.Add(time.Duration(dispatchKeep+dispatchPruneMargin)*time.Minute), 1)
	if after := list(); len(after) != dispatchKeep {
		t.Fatalf("above boundary: got %d records, want %d: %v", len(after), dispatchKeep, after)
	}
	got, err := LoadRecord(active.ID)
	if err != nil {
		t.Fatalf("oldest active record must survive pruning: %v", err)
	}
	if !got.Alive() || got.PID != active.PID {
		t.Fatalf("oldest record no longer active: %+v", got)
	}
}

// A fresh --background record (running, Log set, live PID) reads back as running and Alive, not lost.
func TestFreshBackgroundRecordReadsBackAlive(t *testing.T) {
	setConfigDir(t)
	rec := NewRecord("codex", RunOptions{Prompt: "p"}, time.Now())
	rec.PID, rec.Log = os.Getpid(), "job.log"
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRecord(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "running" || !got.Alive() {
		t.Errorf("read back status=%q alive=%v, want running/true", got.Status, got.Alive())
	}
}

// The record keeps the original request, and the model a fallback hop was asked to run survives Finish even when
// the agent reports no model; the reported Model stays empty rather than echoing the invocation.
func TestRecordKeepsRequestAndInvokedModel(t *testing.T) {
	setConfigDir(t)
	t.Setenv("HOME", t.TempDir())
	first := &recordRunner{name: "codex", err: &retry.Error{Kind: retry.KindQuota, Agent: "codex", Err: io.EOF}}
	second := &recordRunner{name: "claude", res: wire.Result{Answer: "ok"}}
	chain := Chain{Want: "codex", Dispatches: []Dispatch{
		{Agent: first, Model: "prof:gpt-6-astra"},
		{Agent: second, Model: "prof:claude-sonnet-5-5"},
	}}
	req := RunOptions{Prompt: "p", Model: "gpt-6-astra"}
	rec := NewRecord("codex", req, time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC))
	res, err := runWithFallback(context.Background(), chain, req, wire.Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec.Finish(res, err, time.Second)
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRecord(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Request.Model != "gpt-6-astra" {
		t.Errorf("request model = %q, the retry source must stay the original", got.Request.Model)
	}
	if got.Invoked != "prof:claude-sonnet-5-5" || got.RanAs != "claude" {
		t.Errorf("invoked %q ran_as %q, want the fallback hop's model", got.Invoked, got.RanAs)
	}
	if got.Model != "" {
		t.Errorf("model = %q: an agent that reported none must not be filled from the invocation", got.Model)
	}
}

func writeRawDispatch(t *testing.T, id, body string) {
	t.Helper()
	if err := os.MkdirAll(paths.DispatchDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.DispatchDir(), id+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestDispatchRecordSchemaStampsNewAndRejectsNewer pins: unstamped record reads, new record is stamped 1, schema 2 is skipped/errors.
func TestDispatchRecordSchemaStampsNewAndRejectsNewer(t *testing.T) {
	setConfigDir(t)
	const oldID, newerID = "20260101T000000.000-codex", "20260102T000000.000-codex"
	writeRawDispatch(t, oldID, `{"id":"`+oldID+`","time":"2026-01-01T00:00:00Z","agent":"codex","request":{},"status":"ok"}`)
	got, err := LoadRecord(oldID)
	if err != nil || got.ID != oldID || got.Status != "ok" {
		t.Fatalf("pre-schema record: %+v, %v", got, err)
	}

	rec := NewRecord("copilot", RunOptions{}, time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC))
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	assertDispatchSchemaStamp(t, rec.ID)

	writeRawDispatch(t, newerID, `{"schema":2,"id":"`+newerID+`","agent":"codex","status":"ok"}`)
	if _, err := LoadRecord(newerID); !errors.Is(err, schemas.ErrNewer) {
		t.Errorf("LoadRecord(schema 2) error = %v, want wrapping schemas.ErrNewer", err)
	}
	for _, r := range RecentRecords(10) {
		if r.ID == newerID {
			t.Errorf("RecentRecords listed the schema-2 record")
		}
	}
	if n := len(RecentRecords(10)); n != 2 {
		t.Errorf("RecentRecords = %d, want 2 (old + new)", n)
	}
}

func assertDispatchSchemaStamp(t *testing.T, id string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(paths.DispatchDir(), id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["schema"] != float64(1) {
		t.Errorf(`new record "schema" = %v, want 1`, raw["schema"])
	}
}

func TestPrintDispatchLogPreviewCutsOnRuneBoundary(t *testing.T) {
	// Given a prompt of 4-byte runes longer than the preview; When the log prints; Then the cut line is valid UTF-8
	setConfigDir(t)
	rec := NewRecord("codex", RunOptions{Prompt: strings.Repeat("😀", 100)}, time.Now())
	rec.Status = "ok"
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	var out string
	var outSb420 strings.Builder
	for _, r := range RecentRecords(20) {
		outSb420.WriteString(r.LogLine())
	}
	out += outSb420.String()
	if !utf8.ValidString(out) {
		t.Fatalf("output is not valid UTF-8: %q", out)
	}
	if !strings.Contains(out, "😀…") {
		t.Errorf("output lacks a truncated preview ending in …:\n%s", out)
	}
}

// Two batches (separate processes in effect) stamping the same millisecond never share a record id or file.
func TestReserveIsExclusiveAcrossBatches(t *testing.T) {
	setConfigDir(t)
	now := time.Now()
	const n = 8
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			ids[i] = ClaimRecord("codex", RunOptions{Prompt: "p"}, now).ID // the one allocator every dispatch path uses
		})
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("record id %s handed out twice: %v", id, ids)
		}
		seen[id] = true
		if _, err := os.Stat(filepath.Join(paths.DispatchDir(), id+".json")); err != nil {
			t.Errorf("record %s has no claimed file: %v", id, err)
		}
	}
}

// A reserved record is a complete running record from its first byte, so a concurrent prune keeps it.
func TestReservedRecordSurvivesPrune(t *testing.T) {
	setConfigDir(t)
	old := dispatchKeep
	dispatchKeep = 1
	t.Cleanup(func() { dispatchKeep = old })
	dir := paths.DispatchDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	done := Record{Schema: dispatchSchema, ID: "99991231T000000.000-zz", Agent: "zz", Status: "ok"}
	if err := done.Save(); err != nil {
		t.Fatal(err)
	}
	rec := ClaimRecord("aa", RunOptions{Prompt: "p"}, time.Now())
	pruneDispatches(dir) // lands between reserve and Save
	if _, err := os.Stat(filepath.Join(dir, rec.ID+".json")); err != nil {
		t.Errorf("the reserved record was pruned before Save: %v", err)
	}
}

// task_id, batch_id and plan_task survive Save and Load; a record without them still loads.
func TestRecordTaskBatchPlanRoundTrip(t *testing.T) {
	setConfigDir(t)
	r := NewRecord("codex", RunOptions{Prompt: "p"}, time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC))
	r.TaskID, r.BatchID, r.PlanTask = "t1", "batch-x", "Phase 1.1"
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRecord(r.ID)
	if err != nil || got.TaskID != "t1" || got.BatchID != "batch-x" || got.PlanTask != "Phase 1.1" {
		t.Fatalf("loaded = %+v, %v", got, err)
	}
	raw, _ := os.ReadFile(filepath.Join(paths.DispatchDir(), r.ID+".json"))
	for _, key := range []string{`"task_id"`, `"batch_id"`, `"plan_task"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("saved JSON lacks %s: %s", key, raw)
		}
	}

	const oldID = "20260101T000000.000-codex"
	writeRawDispatch(t, oldID, `{"id":"`+oldID+`","agent":"codex","request":{},"status":"ok"}`)
	old, err := LoadRecord(oldID)
	if err != nil || old.Status != "ok" || old.TaskID != "" || old.BatchID != "" || old.PlanTask != "" {
		t.Fatalf("old record = %+v, %v", old, err)
	}
}

// resultProbe records the on-disk record status at the moment the result event is emitted.
type resultProbe struct {
	id     string
	status string
}

func (p *resultProbe) Emit(ev wire.Event) error {
	if ev.Type == wire.EventResult {
		got, _ := LoadRecord(p.id)
		p.status = got.Status
	}
	return nil
}

// A tail follower exits on a finished record, so the result event must be written before the record leaves running.
func TestDispatchWritesResultBeforeFinishedRecord(t *testing.T) {
	setConfigDir(t)
	rec := ClaimRecord("codex", RunOptions{}, time.Now())
	probe := &resultProbe{id: rec.ID}
	_, err := runRecorded(context.Background(), &rec, "codex", probe, RunRecordedOpts{},
		func(context.Context, wire.Sink) (wire.Result, error) {
			return wire.Result{Agent: "codex", Status: wire.StatusOK, Answer: "hi"}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if probe.status != "running" {
		t.Fatalf("record status at result emit = %q, want running", probe.status)
	}
	if got, _ := LoadRecord(rec.ID); got.Status != wire.StatusOK {
		t.Fatalf("final record status = %q, want %q", got.Status, wire.StatusOK)
	}
}

// A codex dispatch asked at sonnet keeps the alias in the record (and so in --retry) while the answer reports the shared model.
func TestRecordKeepsRequestedAlias(t *testing.T) {
	setConfigDir(t)
	rec := NewRecord("codex", RunOptions{Prompt: "p", Model: "sonnet"}, time.Date(2026, 10, 28, 2, 0, 0, 0, time.UTC))
	rec.Finish(wire.Result{Answer: "ok", Model: "gpt-6.1-sol"}, nil, time.Second)
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRecord(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Request.Model != "sonnet" || got.Model != "gpt-6.1-sol" {
		t.Errorf("record request %q model %q: the replay source must stay the alias", got.Request.Model, got.Model)
	}
}

// recordRunner is a stub engine: it returns res and err, standing in for a vendor CLI.
type recordRunner struct {
	name string
	res  wire.Result
	err  error
}

func (f *recordRunner) Name() string { return f.name }

func (f *recordRunner) Caps() Caps {
	return Caps{Model: Native, Effort: Native}
}

func (f *recordRunner) Available() error { return nil }

func (f *recordRunner) Run(context.Context, RunOptions, wire.Sink) (wire.Result, error) {
	return f.res, f.err
}

type brokenSink struct{}

func (brokenSink) Emit(wire.Event) error { return errors.New("stdout closed") }

// Given a sink whose start emit fails, Then the claimed record leaves running instead of staying a phantom.
func TestRunRecordedFailedStartEmitFinishesRecord(t *testing.T) {
	setConfigDir(t)
	rec := ClaimRecord("claude", RunOptions{}, time.Now())
	ran := false
	_, err := runRecorded(context.Background(), &rec, "claude", brokenSink{}, RunRecordedOpts{},
		func(context.Context, wire.Sink) (wire.Result, error) { ran = true; return wire.Result{}, nil })
	if err == nil || ran {
		t.Fatalf("err = %v, ran = %v; want the emit error and no run", err, ran)
	}
	got, lerr := LoadRecord(rec.ID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if got.Status != "error" || got.Error == "" {
		t.Errorf("record = %q/%q, want error with the cause", got.Status, got.Error)
	}
}

func TestRecordSavePersistsOwnProcessIdentity(t *testing.T) {
	setConfigDir(t)
	rec := ClaimRecord("codex", RunOptions{}, time.Now())
	rec.PID = os.Getpid()
	want, err := proc.StartTime(rec.PID)
	if err != nil {
		t.Fatal(err)
	}
	if want == "" {
		t.Fatal("current process has no start time")
	}
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	saved, err := LoadRecord(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.PIDStart != want {
		t.Fatalf("reloaded PIDStart = %q, want current process start time %q", saved.PIDStart, want)
	}
	if !sameStart(&saved) {
		t.Fatal("Cancel's process-identity check rejected the reloaded job's own process")
	}
}

// The dispatch record logs what the request grants beyond the agent's defaults (D3), not what the defaults grant.
func TestNewRecordLogsWidening(t *testing.T) {
	setConfigDir(t)
	vaulttest.Put(t, vault.Overlay, "agents", `{"agents":{"codex":{"permission":{"defaults":{"allow":["Read"]}}}}}`)
	p, err := ParsePermissions("", []string{"Read", "Bash(touch:*)"}, nil, []string{"Bash(rm:*)"})
	if err != nil {
		t.Fatal(err)
	}
	rec := NewRecord("codex", RunOptions{Prompt: "p", Permissions: p}, time.Now())
	if want := []string{"allow Bash(touch:*)"}; !slices.Equal(rec.Widened, want) {
		t.Errorf("Widened = %q, want %q", rec.Widened, want)
	}
}

// A fallback run is judged against the agent that answered; an unreadable fallback default keeps the original list.
func TestFinishRecomputesWideningForFallbackAgent(t *testing.T) {
	setConfigDir(t)
	vaulttest.Put(t, vault.Overlay, "agents",
		`{"agents":{"codex":{"permission":{"defaults":{"allow":["Read"]}}},"gemini":{"permission":{"defaults":{"deny":["Bash("]}}}}}`)
	p, err := ParsePermissions("", []string{"Read"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := NewRecord("codex", RunOptions{Prompt: "p", Permissions: p}, time.Now())
	rec.Finish(wire.Result{Agent: "copilot", Fallback: "codex 429"}, nil, time.Second)
	if want := []string{"allow Read"}; !slices.Equal(rec.Widened, want) {
		t.Errorf("Widened after fallback = %q, want %q", rec.Widened, want)
	}
	rec2 := NewRecord("codex", RunOptions{Prompt: "p", Permissions: p}, time.Now())
	rec2.Finish(wire.Result{Agent: "gemini", Fallback: "codex 429"}, nil, time.Second)
	if len(rec2.Widened) != 0 {
		t.Errorf("bad fallback default overwrote Widened: %q", rec2.Widened)
	}
}

func TestRunRecordedFinalAnswerWriteError(t *testing.T) {
	for _, want := range []error{io.ErrClosedPipe, io.ErrShortWrite} {
		t.Run(want.Error(), func(t *testing.T) {
			setConfigDir(t)
			rec := ClaimRecord("codex", RunOptions{}, time.Now())
			var events collect
			_, err := runRecorded(context.Background(), &rec, "codex", &events,
				RunRecordedOpts{AnswerTo: partialErrorWriter{want}},
				func(context.Context, wire.Sink) (wire.Result, error) {
					return wire.Result{Agent: "codex", Status: wire.StatusOK, Answer: "final answer"}, nil
				})
			if !errors.Is(err, want) || !strings.Contains(err.Error(), "write final answer") {
				t.Fatalf("answer write error = %v, want wrapped %v", err, want)
			}
			assertAnswerWriteFailure(t, rec.ID, events.got)
		})
	}
}

func assertAnswerWriteFailure(t *testing.T, id string, events []wire.Event) {
	t.Helper()
	got, err := LoadRecord(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "error" || !strings.Contains(got.Error, "write final answer") {
		t.Fatalf("saved answer failure = %s/%q", got.Status, got.Error)
	}
	last := events[len(events)-1]
	if last.Type != wire.EventResult || last.Envelope == nil || last.Envelope.Error == nil {
		t.Fatalf("result event = %+v, want the answer write error in its envelope", last)
	}
	if !strings.Contains(last.Envelope.Error.Message, "write final answer") {
		t.Fatalf("result error = %q, want the answer write failure", last.Envelope.Error.Message)
	}
}
