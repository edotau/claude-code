package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/paths"

	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/shapes"
)

// A record saves as running, finishes with its outcome, loads by prefix, and the log prunes the oldest.
func TestDispatchRecordLifecycleAndPrune(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	base := time.Date(2026, 10, 16, 21, 0, 0, 0, time.UTC)
	r := NewRecord("codex", RunOptions{Prompt: "p", Tools: true}, base)
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
	if fi, _ := os.Stat(filepath.Join(paths.DispatchDir(), r.ID+".json")); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	old := dispatchKeep
	dispatchKeep = 2
	defer func() { dispatchKeep = old }()
	for i := 1; i <= 3; i++ {
		_ = NewRecord("gemini", RunOptions{Prompt: "q"}, base.Add(time.Duration(i)*time.Minute)).Save()
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

	// second record pushes the first past dispatchKeep=1, triggering prune
	if err := NewRecord("codex", RunOptions{Prompt: "q"}, base.Add(time.Minute)).Save(); err != nil {
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

	// 4th record with dispatchKeep=2 sets excess=2: without the guard, the walk would consume that
	// excess on `finished` then `active` and leave `trigger1` alone; proving teeth: it does not.
	trigger2 := NewRecord("codex", RunOptions{Prompt: "newest"}, base.Add(3*time.Minute))
	trigger2.Status = "ok"
	if err := trigger2.Save(); err != nil {
		t.Fatal(err)
	}

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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
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

	if err := NewRecord("codex", RunOptions{Prompt: "new"}, base.Add(time.Minute)).Save(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(paths.DispatchDir(), dead.ID+".json")); !os.IsNotExist(err) {
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

	fg := NewRecord("codex", RunOptions{Prompt: "fg"}, base) // Status "running", Log left empty
	if err := fg.Save(); err != nil {
		t.Fatal(err)
	}

	if err := NewRecord("codex", RunOptions{Prompt: "new"}, base.Add(time.Minute)).Save(); err != nil {
		t.Fatal(err)
	}

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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
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
	trigger2 := NewRecord("codex", RunOptions{Prompt: "trigger2"}, base.Add(2*time.Minute))
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

// A fresh --background record (running, Log set, live PID) reads back as running and Alive, not lost.
func TestFreshBackgroundRecordReadsBackAlive(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	first := &pinFallbackRunner{name: "codex", err: &Error{Kind: KindQuota, Agent: "codex", Err: io.EOF}}
	second := &pinFallbackRunner{name: "claude", res: Result{Answer: "ok"}}
	chain := Chain{Want: "codex", Dispatches: []Dispatch{
		{Agent: first, Model: "prof:gpt-6-astra"},
		{Agent: second, Model: "prof:claude-sonnet-5-5"},
	}}
	req := RunOptions{Prompt: "p", Model: "gpt-6-astra"}
	rec := NewRecord("codex", req, time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC))
	res, err := RunWithFallback(context.Background(), chain, req, Discard, nil)
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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
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
	b, err := os.ReadFile(filepath.Join(paths.DispatchDir(), rec.ID+".json"))
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

func TestPrintDispatchLogPreviewCutsOnRuneBoundary(t *testing.T) {
	// Given a prompt of 4-byte runes longer than the preview; When the log prints; Then the cut line is valid UTF-8
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	old := dispatchKeep
	dispatchKeep = 1
	t.Cleanup(func() { dispatchKeep = old })
	dir := paths.DispatchDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	done := Record{Schema: DispatchSchema, ID: "99991231T000000.000-zz", Agent: "zz", Status: "ok"}
	if err := done.Save(); err != nil {
		t.Fatal(err)
	}
	rec := ClaimRecord("aa", RunOptions{Prompt: "p"}, time.Now())
	pruneDispatches(dir) // lands between reserve and Save
	if _, err := os.Stat(filepath.Join(dir, rec.ID+".json")); err != nil {
		t.Errorf("the reserved record was pruned before Save: %v", err)
	}
}

const evSecret = "AKIAIOSFODNN7EXAMPLE"

// emitScript is a fake vendor body that drains stdin then prints each NDJSON line on stdout.
func emitScript(lines ...string) string {
	var b strings.Builder
	b.WriteString("cat > /dev/null\n")
	for _, l := range lines {
		b.WriteString("printf '%s\\n' '" + l + "'\n")
	}
	return b.String()
}

// loggedRun runs fn under a WithLog (or, for a job run, WithLogEvents) ctx and returns the closed log's content.
func loggedRun(t *testing.T, jobMode bool, fn func(ctx context.Context)) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "d.log")
	sink := NewLogSink(path)
	ctx := WithLog(context.Background(), sink)
	if jobMode {
		ctx = WithLogEvents(context.Background(), sink)
	}
	fn(ctx)
	sink.Close()
	return readLog(t, path)
}

func TestLogEventOpenCodeFormatted(t *testing.T) {
	r, err := Lookup("opencode")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE_API_KEY", "test-key")
	var out strings.Builder
	log := loggedRun(t, false, func(ctx context.Context) {
		pathstest.FakeVendor(t, "opencode", emitScript(
			`{"type":"text","sessionID":"ses_A","part":{"text":"hello AWS_SECRET=`+evSecret+`"}}`,
			`{"type":"step_finish","sessionID":"ses_A","part":{"tokens":{"input":3,"output":2}}}`))
		if _, err := r.Run(ctx, RunOptions{Prompt: "p", WorkDir: t.TempDir()}, NewTextSink(&out)); err != nil {
			t.Fatalf("Run: %v", err)
		}
	})
	if out.String() != "hello AWS_SECRET="+evSecret {
		t.Errorf("caller stream = %q", out.String())
	}
	// step_finish may or may not log a result line; the assistant line is exact and any extra line is formatted.
	lines := strings.Split(strings.TrimRight(log, "\n"), "\n")
	if lines[0] != "▸ assistant: hello AWS_SECRET=REDACTED" || len(lines) > 2 {
		t.Errorf("log lines = %q, want the assistant line first", lines)
	}
	if len(lines) == 2 && !strings.HasPrefix(lines[1], "▸ result:") {
		t.Errorf("second log line = %q, want a '▸ result:' line", lines[1])
	}
	if strings.Contains(log, evSecret) || strings.Contains(log, `{"`) {
		t.Errorf("log leaks the secret or raw JSON:\n%s", log)
	}
}

// The plain-text vendor path (codex etc.) tees stdout via teeWriter: a job carrier must not tee it, a plain
// WithLog still must, and both keep feeding stderr/event lines.
func TestLogEventJobModeNoRawStdoutTee(t *testing.T) {
	for _, tc := range []struct {
		name    string
		job     bool
		wantRaw bool
	}{{"job carrier", true, false}, {"plain WithLog", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			var caller strings.Builder
			log := loggedRun(t, tc.job, func(ctx context.Context) {
				if stderrLogFrom(ctx) == nil {
					t.Fatal("stderrLogFrom lost the sink")
				}
				w := TeeWriter(ctx, &caller)
				_, _ = io.WriteString(w, "raw-stdout-answer\n")
				logEvent(ctx, "result", "ev-line")
			})
			if caller.String() != "raw-stdout-answer\n" {
				t.Errorf("caller stream = %q", caller.String())
			}
			if got := strings.Contains(log, "raw-stdout-answer"); got != tc.wantRaw {
				t.Errorf("raw stdout in log = %v, want %v:\n%s", got, tc.wantRaw, log)
			}
			if !strings.Contains(log, "▸ result: ev-line") {
				t.Errorf("event line missing:\n%s", log)
			}
		})
	}
}

// readLog reads path, failing the test on error.
func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return string(b)
}

// TestLogSinkModeAppendRedact: the log is 0600, a second sink appends, and a secret split across Writes is redacted.
func TestLogSinkModeAppendRedact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	s1 := NewLogSink(path)
	_, _ = s1.Stream().Write([]byte("first\n"))
	s1.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Errorf("log mode = %o, want 600", mode)
	}

	token := "ant-api03-abcdefghijklmnopqrstuvwxyz0123456789"
	line := "key=sk-" + token + "\n"
	if shapes.Redact(line) == line {
		t.Fatalf("shapes.Redact(%q) left it unchanged; test has no teeth", line)
	}
	s2 := NewLogSink(path)
	w := s2.Stream()
	_, _ = w.Write([]byte("key=sk-"))
	_, _ = w.Write([]byte(token))
	_, _ = w.Write([]byte("\n"))
	s2.Close()

	got := readLog(t, path)
	if strings.Contains(got, token) {
		t.Errorf("log leaks the raw token: %q", got)
	}
	if want := "first\n" + shapes.Redact(line); got != want {
		t.Errorf("log = %q, want %q", got, want)
	}
}

// TestLogSinkStdoutByteIdentical: a sink must not change what the caller's stdout receives.
func TestLogSinkStdoutByteIdentical(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake")
	script := "#!/bin/sh\nprintf 'alpha\\nbeta\\n'\necho oops >&2\nprintf 'gamma\\nno-newline'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(ctx context.Context) []byte {
		t.Helper()
		var buf bytes.Buffer
		cmd, _ := ChildCmd(ctx, bin, nil, nil, "", "", nil, true)
		cmd.Stdout = TeeWriter(ctx, &buf)
		if err := cmd.Run(); err != nil {
			t.Fatalf("run fake: %v", err)
		}
		return buf.Bytes()
	}
	plain := run(context.Background())
	logPath := filepath.Join(dir, "job.log")
	sink := NewLogSink(logPath)
	teed := run(WithLog(context.Background(), sink))
	sink.Close()

	if !bytes.Equal(plain, teed) {
		t.Errorf("stdout differs with a sink:\nplain %q\nteed  %q", plain, teed)
	}
	if want := "alpha\nbeta\ngamma\nno-newline"; string(plain) != want {
		t.Errorf("stdout = %q, want %q", plain, want)
	}
	log := readLog(t, logPath)
	for _, l := range []string{"alpha\n", "beta\n", "gamma\n", "no-newline\n", "oops\n"} {
		if !strings.Contains(log, l) {
			t.Errorf("log %q missing %q", log, l)
		}
	}
}

// TestLogSinkPartialLineFlushedOnClose: an unterminated last line waits for Close, which adds the newline.
func TestLogSinkPartialLineFlushedOnClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	s := NewLogSink(path)
	_, _ = s.Stream().Write([]byte("whole\npartial"))
	if got := readLog(t, path); got != "whole\n" {
		t.Errorf("log before Close = %q, want %q", got, "whole\n")
	}
	s.Close()
	if got := readLog(t, path); got != "whole\npartial\n" {
		t.Errorf("log after Close = %q, want %q", got, "whole\npartial\n")
	}
}

// TestLogSinkLazyCreate: NewLogSink alone creates no file; the first Stream does.
func TestLogSinkLazyCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	s := NewLogSink(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Stat before Stream err = %v, want not-exist", err)
	}
	s.Stream()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("Stat after Stream: %v", err)
	}
	s.Close()
}

// TestLogSinkConcurrentStreamsNoInterleave: two streams written concurrently keep every log line whole.
func TestLogSinkConcurrentStreamsNoInterleave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	s := NewLogSink(path)
	const n = 500
	want := map[string]bool{}
	var wg sync.WaitGroup
	for _, name := range []string{"stdout", "stderr"} {
		w := s.Stream()
		for i := range n {
			want[fmt.Sprintf("%s line %d %s", name, i, strings.Repeat(name[3:], 20))] = true
		}
		wg.Go(func() {
			for i := range n {
				l := fmt.Sprintf("%s line %d %s\n", name, i, strings.Repeat(name[3:], 20))
				_, _ = w.Write([]byte(l[:len(l)/2])) // split each line across two Writes
				_, _ = w.Write([]byte(l[len(l)/2:]))
			}
		})
	}
	wg.Wait()
	s.Close()

	lines := strings.Split(strings.TrimSuffix(readLog(t, path), "\n"), "\n")
	if len(lines) != 2*n {
		t.Fatalf("log has %d lines, want %d", len(lines), 2*n)
	}
	for _, l := range lines {
		if !want[l] {
			t.Fatalf("log line %q is not a whole expected line", l)
		}
		delete(want, l)
	}
}
