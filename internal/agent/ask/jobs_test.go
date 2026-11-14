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
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// A failed job's stderr tail may carry a key; result must not echo it raw.
func TestResultExitRedactsError(t *testing.T) {
	const key = "sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789ABCDEF"
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
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv("PATH", dir)
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
		return askPrepareAndRun(context.Background(), fs, req, job, agentconfig.Overlay{}, nil, nil, "", "json", "codex", mode == "background", false, nil, io.Discard)
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

// result -f streams the log as the job writes it, then closes with the ask line and resume hint without repeating the log.
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

// No id: the agent's newest finished job in this repo; a bare id finds its agent from the record; neither is a usage error.
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
