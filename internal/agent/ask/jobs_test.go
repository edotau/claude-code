package ask

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
	"github.com/edotau/claude-code/internal/worktree"
)

// A failed job's stderr tail may carry a key; result must not echo it raw.
func TestResultExitRedactsError(t *testing.T) {
	const key = "sk" + "-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789ABCDEF"
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	rc := resultExit(agent.Record{Status: "error", Error: "401 bad key " + key})
	os.Stderr = orig
	w.Close()
	out, _ := io.ReadAll(r)
	if rc != 1 || strings.Contains(string(out), key) {
		t.Errorf("rc=%d stderr=%q; want 1 and no raw key", rc, out)
	}
}

// A job that finished on its own between the read and the cancel is reported as finished, not cancelled.
func TestCancelJobReportsConcurrentFinish(t *testing.T) {
	const id = "20261118T150806.000-claude"
	old := cancelDispatch
	t.Cleanup(func() { cancelDispatch = old })
	cancelDispatch = func(context.Context, string, string, string) (agent.CancelResult, error) {
		return agent.CancelResult{ID: id, Status: "error", AlreadyFinished: true}, nil
	}
	rc, out, _ := hosttest.CaptureOutput(t, func() int { return CancelJob("agents cancel", "claude", id, "") })
	if want := id + " already finished: error\n"; rc != 0 || out != want {
		t.Errorf("rc=%d stdout=%q, want 0 and %q", rc, out, want)
	}
}

func TestLiveProviderRetainedInHolderRecord(t *testing.T) {
	vaulttest.Use(t)
	cliEnv(t)
	cliFakes(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"live-test":{"preset":"databricks","host":"https://test.cloud.databricks.com","auth":{"type":"pat"}}}}}`)
	vaulttest.Secret(t, "DATABRICKS_LIVE_TEST_PAT", "dapi-test")
	code, out, stderr := runCLI(t, "ask", "--live", "--agent", "gemini", "--provider", "databricks", "--format", "json")
	if code != 1 {
		t.Fatalf("exit %d, want refused test-binary spawn\n%s\n%s", code, out, stderr)
	}
	recs := agent.RecentRecords(2)
	if len(recs) != 1 {
		t.Fatalf("records = %+v, want one saved holder request\n%s\n%s", recs, out, stderr)
	}
	if !recs[0].Live || recs[0].Request.Provider != "live-test" || recs[0].Request.Lead != "" { // live Build reads Provider
		t.Fatalf("holder request = %+v, want provider live-test and no lead", recs[0])
	}
}

func TestAbandonLiveReportsCancelFailureWithoutMarkingRecord(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := agent.NewRecord("claude", agent.RunOptions{Prompt: "hi"}, time.Now())
	rec.PID = os.Getpid() // Cancel refuses this non-holder process before sending any signal.
	rec.Log = filepath.Join(t.TempDir(), "holder.log")
	rec.Status = "running"
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("socket never appeared")
	err := abandonLive(&rec, cause)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "stop failed") {
		t.Fatalf("abandonLive = %v, want readiness cause and cancel failure", err)
	}
	got, err := agent.LoadRecord(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != "running" || got.Status != "running" || got.Error != "" {
		t.Fatalf("record = %+v (in-memory status %q), want unchanged running record", got, rec.Status)
	}
}

// A holder that never became ready leaves its record marked error with the cause, not running.
func TestAbandonLiveMarksRecordFailed(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := agent.NewRecord("claude", agent.RunOptions{Prompt: "hi"}, time.Now())
	rec.Status = "running"
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	abandonLive(&rec, errors.New("socket never appeared"))
	got, err := agent.LoadRecord(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "error" || got.Error != "socket never appeared" {
		t.Errorf("record = status %q error %q, want error / cause", got.Status, got.Error)
	}
}

// A live session keeps --read-only and the permission rules: tools-on alone would grant full write access.
func TestLiveRequestCarriesReadOnlyAndPermissions(t *testing.T) {
	perms, err := agent.ParsePermissions("plan", []string{"Bash(ls:*)"}, nil, []string{"Bash(rm:*)"})
	if err != nil {
		t.Fatal(err)
	}
	f := askFlags{readOnly: true, tools: true, noTools: true, fleet: true, enableMemory: true}
	s := (&askRun{f: &f}).askState()
	got := liveRequest(t.Context(), s, perms, "claude")
	if !got.ReadOnly || !got.Tools || !got.NoTools || !got.Fleet || !got.EnableMemory {
		t.Errorf("live request lost a flag: %+v", got)
	}
	if !reflect.DeepEqual(got.Permissions, perms) {
		t.Errorf("Permissions = %+v, want %+v", got.Permissions, perms)
	}
}

// A live holder runs one hop with no chain: an inherited confinement's profile beats the typed or pinned one.
func TestLiveRequestStartsOnConfinedProfile(t *testing.T) {
	vaulttest.Use(t)
	for _, tc := range []struct{ confinement, typed, want string }{
		{"databricks:qa", "dev", "qa"},
		{"openai:acme", "", "acme"},
		{"", "dev", "dev"}, // unconfined: the typed profile stands
	} {
		confine(t, tc.confinement)
		if tc.confinement == "" {
			t.Setenv(agentconfig.OverlayVar, "")
		}
		f := askFlags{provider: tc.typed}
		if got := liveRequest(t.Context(), (&askRun{f: &f}).askState(), agent.Permissions{}, "gemini").Provider; got != tc.want {
			t.Errorf("confinement %q, typed %q: Provider = %q, want %q", tc.confinement, tc.typed, got, tc.want)
		}
	}
}

// --live --as sp logs the identity on the request the holder will serve (the spawn itself is refused under go test).
func TestStartLiveRecordsIdentity(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh script")
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv("PATH", dir)
	t.Setenv(paths.AgentBinDirEnv, filepath.Join(dir, "bin"))
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "claude"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := askFlags{}
	s := (&askRun{f: &f}).askState()
	if code, _, _ := hosttest.CaptureOutput(t, func() int { return startLive(t.Context(), s, agent.Permissions{}, "claude", "sp", "json") }); code != 1 {
		t.Fatalf("startLive = %d, want 1 (test binary is not relaunched)", code)
	}
	recs := agent.RecentRecords(1)
	if len(recs) != 1 || recs[0].Request.Identity != "sp" || !recs[0].Live {
		t.Fatalf("records = %+v, want one live record with Identity sp", recs)
	}
}

// Unsupported requests refuse at every lifecycle boundary before a record or child exists.
func TestPermissionLifecycleRefusesUnsupported(t *testing.T) {
	vaulttest.Use(t)
	cliEnv(t)
	for _, mode := range []string{"foreground", "background", "job", "batch", "live"} {
		t.Run(mode, func(t *testing.T) {
			checkPermissionBoundary(t, mode)
			if recs := agent.RecentRecords(10); len(recs) != 0 {
				t.Fatalf("%s saved records before refusal: %+v", mode, recs)
			}
		})
	}
}

func checkPermissionBoundary(t *testing.T, mode string) {
	t.Helper()
	req := agent.RunOptions{Agent: "codex", Prompt: "proof", NoTools: true}
	switch mode {
	case "batch":
		checkBatchBoundary(t, req)
	default:
		checkCLIBoundary(t, mode, req)
	}
}

func checkBatchBoundary(t *testing.T, req agent.RunOptions) {
	t.Helper()
	_, _, err := lineRequest(parsedLine{N: 1, Task: batchLine{Prompt: "proof"}}, "codex", req)
	if err == nil || !strings.Contains(err.Error(), "--no-tools") {
		t.Fatalf("batch refusal = %v", err)
	}
}

// checkCLIBoundary runs the foreground, background, job or live entry and expects exit 2 naming --no-tools.
func checkCLIBoundary(t *testing.T, mode string, req agent.RunOptions) {
	t.Helper()
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		if mode == "live" {
			f := askFlags{noTools: true}
			return startLive(t.Context(), (&askRun{f: &f}).askState(), agent.Permissions{}, "codex", "", "json")
		}
		fs, _ := askFlagSet()
		var job *agent.Record
		if mode == "job" {
			job = &agent.Record{Request: req}
		}
		return askPrepareAndRun(context.Background(), fs, req, job, agentconfig.Overlay{}, nil, nil, "", "json", "", "codex", mode == "background", false, nil, io.Discard)
	})
	if code != 2 || !strings.Contains(stderr, "--no-tools") {
		t.Fatalf("%s refusal = %d, %s", mode, code, stderr)
	}
}

// jobsEnv gives the test its own dispatch store and a working directory that is its repo.
func jobsEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Chdir(t.TempDir())
}

// seedJob saves a codex record in this repo whose log holds body; a running one gets pid (this process when 0).
func seedJob(t *testing.T, age time.Duration, status, body string, pid int) agent.Record {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rec := agent.NewRecord("codex", agent.RunOptions{Prompt: "seed " + status}, time.Now().Add(-age))
	rec.Repo, rec.Status, rec.Log = cwd, status, filepath.Join(cwd, rec.ID+".log")
	if status == "running" {
		rec.PID = pid
	}
	if err := os.WriteFile(rec.Log, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	return rec
}

// finishAfter appends more to rec's log, then finishes it with runErr, once d has passed.
func finishAfter(rec agent.Record, d time.Duration, more string, runErr error) <-chan error {
	done := make(chan error, 1)
	go func() {
		time.Sleep(d)
		f, err := os.OpenFile(rec.Log, os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			_, err = f.WriteString(more)
			err = errors.Join(err, f.Close())
		}
		rec.Finish(wire.Result{SessionID: "sess-1"}, runErr, d)
		done <- errors.Join(err, rec.Save())
	}()
	return done
}

func runResult(t *testing.T, o ResultOptions) (code int, stdout, stderr string) {
	t.Helper()
	return hosttest.CaptureOutput(t, func() int { return Result(o) })
}

// result -f streams the log as the job writes it, then closes with the ask line and resume hint, not the log again.
func TestResultFollowStreamsLogThenTrailer(t *testing.T) {
	jobsEnv(t)
	rec := seedJob(t, time.Minute, "running", "first\n", os.Getpid())
	done := finishAfter(rec, 100*time.Millisecond, "second\n", nil)
	code, out, stderr := runResult(t, ResultOptions{Agent: "codex", Follow: true})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d stderr %q, want 0", code, stderr)
	}
	if !strings.HasPrefix(out, "first\nsecond\n") || strings.Count(out, "first") != 1 || !strings.Contains(out, "session: sess-1") {
		t.Errorf("stdout = %q, want the log once, then the session trailer", out)
	}
}

// A followed job that errors exits 1 with its error, and a followed job whose process vanished is reported lost.
func TestResultFollowErroredAndLost(t *testing.T) {
	hosttest.SkipOnWindows(t, "unix tools")
	jobsEnv(t)
	rec := seedJob(t, time.Minute, "running", "work\n", os.Getpid())
	done := finishAfter(rec, 50*time.Millisecond, "", errors.New("boom"))
	code, _, stderr := runResult(t, ResultOptions{Agent: "codex", Follow: true})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if code != 1 || !strings.Contains(stderr, "boom") {
		t.Errorf("errored: exit %d stderr %q, want 1 naming boom", code, stderr)
	}

	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	lost := seedJob(t, 0, "running", "", gone.Process.Pid)
	code, _, stderr = runResult(t, ResultOptions{Agent: "codex", Ref: lost.ID, Follow: true})
	if code != 1 || !strings.Contains(stderr, "is lost") {
		t.Errorf("lost: exit %d stderr %q, want 1 and \"is lost\"", code, stderr)
	}
}

// With json the envelope is the only output: a followed job waits for the end and streams no log ahead of it.
func TestResultFollowJSONPrintsOnlyEnvelope(t *testing.T) {
	jobsEnv(t)
	rec := seedJob(t, time.Minute, "running", "first\n", os.Getpid())
	done := finishAfter(rec, 50*time.Millisecond, "second\n", nil)
	code, out, _ := runResult(t, ResultOptions{Agent: "codex", Follow: true, Format: "json"})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if code != 0 || !json.Valid([]byte(out)) {
		t.Errorf("exit %d stdout %q, want exactly one JSON envelope", code, out)
	}
}

// --events copies the NDJSON event file as is, with no log trailer.
func TestResultEventsStreamsEventFile(t *testing.T) {
	jobsEnv(t)
	rec := seedJob(t, time.Minute, "ok", "log text\n", 0)
	const events = "{\"type\":\"a\"}\n{\"type\":\"b\"}\n"
	if err := os.WriteFile(rec.EventsPath(), []byte(events), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runResult(t, ResultOptions{Agent: "codex", Ref: rec.ID, Events: true})
	if code != 0 || out != events {
		t.Errorf("exit %d stdout %q, want the event file %q", code, out, events)
	}
}

// No id: the agent's newest finished job in this repo; a bare id finds its agent from the record; else a usage error.
func TestResultSelectsJob(t *testing.T) {
	jobsEnv(t)
	old := seedJob(t, 3*time.Minute, "ok", "old\n", 0)
	seedJob(t, 2*time.Minute, "ok", "new\n", 0)
	seedJob(t, time.Minute, "running", "busy\n", os.Getpid())
	for _, tc := range []struct {
		name string
		o    ResultOptions
		code int
		want string
	}{
		{"agent newest finished", ResultOptions{Agent: "codex"}, 0, "new\n"},
		{"bare id", ResultOptions{Ref: old.ID}, 0, "old\n"},
		{"neither", ResultOptions{}, 2, ""},
	} {
		code, out, _ := runResult(t, tc.o)
		if code != tc.code || !strings.HasPrefix(out, tc.want) || (tc.want != "" && strings.Contains(out, "busy")) {
			t.Errorf("%s: exit %d stdout %q, want exit %d starting %q", tc.name, code, out, tc.code, tc.want)
		}
	}
}

// Bare `agents status` lists recent dispatches as LogLine rows, or json records; an empty store says so.
func TestDispatchLogTextAndJSON(t *testing.T) {
	jobsEnv(t)
	if _, out, _ := hosttest.CaptureOutput(t, func() int { return DispatchLog("agents status", "text") }); !strings.Contains(out, "no dispatches logged yet") {
		t.Errorf("empty store stdout = %q, want the empty notice", out)
	}
	a := seedJob(t, 2*time.Minute, "ok", "", 0)
	b := seedJob(t, time.Minute, "error", "", 0)
	_, out, _ := hosttest.CaptureOutput(t, func() int { return DispatchLog("agents status", "text") })
	if !strings.Contains(out, a.ID) || !strings.Contains(out, b.ID) {
		t.Errorf("text stdout = %q, want both dispatch ids", out)
	}
	_, out, _ = hosttest.CaptureOutput(t, func() int { return DispatchLog("agents status", "json") })
	var recs []agent.Record
	if err := json.Unmarshal([]byte(out), &recs); err != nil || len(recs) != 2 {
		t.Errorf("json stdout = %q (%v), want two records", out, err)
	}
}

// send resumes a finished dispatch as a child with its posture; an expired wait or session-less one launches nothing.
func TestSendResumesFinishedDispatch(t *testing.T) {
	jobsEnv(t)
	t.Setenv("PATH", t.TempDir()) // no agent binary: the resume is recorded, then fails to start
	t.Setenv("HOME", t.TempDir())
	t.Setenv(proc.DispatchIDEnv, "outer")
	perms, err := agent.ParsePermissions("", []string{"Read"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	req := agent.RunOptions{Prompt: "first", Tools: true, ReadOnly: true, Effort: "low", WorkDir: cwd, Permissions: perms}
	fin := agent.NewRecord("claude", req, time.Now().Add(-3*time.Hour))
	fin.Finish(wire.Result{SessionID: "sess-1"}, nil, time.Second)
	bare := agent.NewRecord("claude", req, time.Now().Add(-2*time.Hour))
	bare.Finish(wire.Result{}, nil, time.Second)
	busy := agent.NewRecord("claude", req, time.Now())
	busy.PID = os.Getpid()
	for _, r := range []agent.Record{fin, bare, busy} {
		if err := r.Save(); err != nil {
			t.Fatal(err)
		}
	}
	assertSendRefused(t, busy.ID, "timeout", 20*time.Millisecond, 3)
	assertSendRefused(t, bare.ID, "no session id", 0, 3)
	hosttest.CaptureOutput(t, func() int { return Send(fin.ID, "follow up", "json", 0) })
	recs := agent.RecentRecords(10)
	if len(recs) != 4 {
		t.Fatalf("%d records, want 4 (the resume is the only new one)", len(recs))
	}
	got := recs[0]
	if got.ID == fin.ID || got.Parent != fin.ID || got.Agent != "claude" || got.Depth != fin.Depth+1 {
		t.Errorf("resume record = id %s agent %s parent %q depth %d; want a new claude child of %s", got.ID, got.Agent, got.Parent, got.Depth, fin.ID)
	}
	assertResumedPosture(t, got.Request, cwd)
}

func assertSendRefused(t *testing.T, id, want string, timeout time.Duration, count int) {
	t.Helper()
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return Send(id, "again", "text", timeout) })
	if code != 1 || !strings.Contains(stderr, want) || len(agent.RecentRecords(10)) != count {
		t.Fatalf("send %s: exit %d, stderr %q, %d records; want exit 1 naming %q and nothing launched",
			id, code, stderr, len(agent.RecentRecords(10)), want)
	}
}

func assertResumedPosture(t *testing.T, r agent.RunOptions, cwd string) {
	t.Helper()
	if r.Prompt != "follow up" || r.SessionID != "sess-1" || !r.ReadOnly || !r.Tools || r.Effort != "low" || r.WorkDir != cwd ||
		!reflect.DeepEqual(agent.RuleStrings(r.Permissions.Allow), []string{"Read"}) {
		t.Errorf("resume request = %+v; want the record's session, tools, read-only, effort, workdir and permissions with the new prompt", r)
	}
}

func TestSendQueuesNoteUntilJobEnds(t *testing.T) {
	jobsEnv(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	rec := agent.NewRecord("claude", agent.RunOptions{Prompt: "first"}, time.Now())
	rec.PID = os.Getpid()
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		rec.Finish(wire.Result{SessionID: "sess-1"}, errors.New("original job failed"), time.Second)
		finished <- rec.Save()
	}()
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return Send(rec.ID, "queued note", "json", 2*time.Second)
	})
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stderr, "job "+rec.ID+" running; note queued, resumes when it ends\n") {
		t.Errorf("stderr = %q, want queued notice first", stderr)
	}
	recs := agent.RecentRecords(2)
	if len(recs) != 2 || recs[0].Parent != rec.ID || recs[0].Request.Prompt != "queued note" ||
		recs[0].Request.SessionID != "sess-1" {
		t.Fatalf("resume records = %+v, want a child with the queued note and session", recs)
	}
	if code != 1 {
		t.Errorf("exit = %d, want child dispatch failure (no agent binary)", code)
	}
}

func TestSendParentFilesTheLiteralNoteOrSaysNoDispatcher(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(proc.DispatchIDEnv, "")
	t.Setenv(memory.SessionIDEnv, "main-sess")
	reg := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "sessions")
	pid := strconv.Itoa(os.Getpid())
	if err := os.MkdirAll(reg, 0o700); err != nil || os.WriteFile(filepath.Join(reg, pid+".json"), []byte(`{"pid":`+pid+`,"sessionId":"main-sess"}`), 0o600) != nil {
		t.Fatal("register session", err)
	}
	for _, note := range []string{"done with x.go", "-", "@notes.txt"} {
		if code := Send(ParentTarget, note, "text", 0); code != 0 {
			t.Fatalf("Send parent %q = %d, want 0", note, code)
		}
	}
	got, err := worktree.DrainErr("main-sess")
	if err != nil || len(got) != 3 || worktree.NoteText(got[0]) != "done with x.go" || worktree.NoteText(got[1]) != "-" || worktree.NoteText(got[2]) != "@notes.txt" {
		t.Fatalf("inbox = %+v, %v; want the three literal notes", got, err)
	}

	t.Setenv(memory.SessionIDEnv, "")
	if code := Send(ParentTarget, "nobody", "text", 0); code != 0 {
		t.Errorf("no parent: code %d, want 0", code)
	}
}

// A stalled, partial or quota job is a failure to `agents result`, like an error job.
func TestResultExitFailsCutoffStatuses(t *testing.T) {
	for _, st := range []string{"error", "stalled", "partial", "quota"} {
		if rc := resultExit(agent.Record{Status: st, Error: "cut off"}); rc != 1 {
			t.Errorf("status %q: rc = %d, want 1", st, rc)
		}
	}
	if rc := resultExit(agent.Record{Status: "ok"}); rc != 0 {
		t.Errorf("ok job: rc = %d, want 0", rc)
	}
}
