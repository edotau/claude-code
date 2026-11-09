package ask

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"

	"github.com/edotau/claude-code/internal/agent/job"

	"github.com/edotau/claude-code/internal/agent"
)

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
	recs := job.RecentRecords(2)
	if len(recs) != 1 {
		t.Fatalf("records = %+v, want one saved holder request\n%s\n%s", recs, out, stderr)
	}
	if !recs[0].Live || recs[0].Request.Provider != "live-test" || recs[0].Request.Lead != "" { // live Build reads Provider
		t.Fatalf("holder request = %+v, want provider live-test and no lead", recs[0])
	}
}

func TestAbandonLiveReportsCancelFailureWithoutMarkingRecord(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := job.NewRecord("claude", agent.RunOptions{Prompt: "hi"}, time.Now())
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
	got, err := job.LoadRecord(rec.ID)
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
	rec := job.NewRecord("claude", agent.RunOptions{Prompt: "hi"}, time.Now())
	rec.Status = "running"
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	abandonLive(&rec, errors.New("socket never appeared"))
	got, err := job.LoadRecord(rec.ID)
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
	f := askFlags{readOnly: true, tools: true}
	s := (&askRun{f: &f}).askState()
	got := liveRequest(s, perms, "claude")
	if !got.ReadOnly || !got.Tools {
		t.Errorf("ReadOnly=%v Tools=%v, want both true", got.ReadOnly, got.Tools)
	}
	if !reflect.DeepEqual(got.Permissions, perms) {
		t.Errorf("Permissions = %+v, want %+v", got.Permissions, perms)
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
	recs := job.RecentRecords(1)
	if len(recs) != 1 || recs[0].Request.Identity != "sp" || !recs[0].Live {
		t.Fatalf("records = %+v, want one live record with Identity sp", recs)
	}
}
