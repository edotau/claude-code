package ask

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/roles"
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
	got := liveRequest(s, perms, "claude")
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
		if got := liveRequest((&askRun{f: &f}).askState(), agent.Permissions{}, "gemini").Provider; got != tc.want {
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
	if code, _, _ := hosttest.CaptureOutput(t, func() int { return startLive(s, agent.Permissions{}, "claude", "sp", "json") }); code != 1 {
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
	for _, mode := range []string{"foreground", "background", "job", "batch", "live", "dispatch", "detached"} {
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
	case "dispatch":
		checkDispatchBoundary(t, req)
	case "detached":
		checkDetachedBoundary(t, req)
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

func checkDispatchBoundary(t *testing.T, req agent.RunOptions) {
	t.Helper()
	for ev := range legStream(newClient(), "", "none")(context.Background(), req) {
		if ev.Envelope == nil || ev.Envelope.Error == nil || !strings.Contains(ev.Envelope.Error.Message, "--no-tools") {
			t.Fatalf("dispatch event = %+v", ev)
		}
	}
}

func checkDetachedBoundary(t *testing.T, req agent.RunOptions) {
	t.Helper()
	if _, err := spawnLeg(context.Background(), newClient(), roles.Leg{Opts: req}); err == nil || !strings.Contains(err.Error(), "--no-tools") {
		t.Fatalf("detached refusal = %v", err)
	}
}

// checkCLIBoundary runs the foreground, background, job or live entry and expects exit 2 naming --no-tools.
func checkCLIBoundary(t *testing.T, mode string, req agent.RunOptions) {
	t.Helper()
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		if mode == "live" {
			f := askFlags{noTools: true}
			return startLive((&askRun{f: &f}).askState(), agent.Permissions{}, "codex", "", "json")
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
