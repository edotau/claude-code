package agentcmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/hosttest"
)

func saveResultRecord(t *testing.T, status, errMsg string) job.Record {
	t.Helper()
	cwd, _ := os.Getwd()
	rec := job.NewRecord("codex", agent.RunOptions{Prompt: "p"}, time.Now())
	rec.Status, rec.Error = status, errMsg
	rec.Repo = job.RepoRoot(context.Background(), cwd)
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestVendorResultErrorRecordExits1(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := saveResultRecord(t, "error", "boom happened")
	for _, args := range [][]string{{rec.ID}, {"--format", "json", rec.ID}} {
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return agentResult("codex", args) })
		if code != 1 || !strings.Contains(stderr, "error: boom happened") {
			t.Errorf("args %v: exit %d stderr %q, want 1 and \"error: boom happened\"", args, code, stderr)
		}
	}
}

func TestVendorResultOKRecordExits0(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := saveResultRecord(t, "ok", "")
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return agentResult("codex", []string{rec.ID}) })
	if code != 0 || strings.Contains(stderr, "error:") {
		t.Errorf("exit %d stderr %q, want 0 and no error line", code, stderr)
	}
}

// TestVendorVerbRoutesSetupToCmdVendorSetup: cmdVendorSetup prints its report to stdout (not an
// error), so unlike the other handlers its routing proof reads stdout, keyed on rep.Vendor appearing
// as the row's first column.
func TestVendorVerbRoutesSetupToCmdVendorSetup(t *testing.T) {
	_, stdout, _ := hosttest.CaptureOutput(t, func() int { return agentVerb(notAVendor, []string{"setup"}) })
	if !strings.Contains(stdout, notAVendor+"  agent  fail") {
		t.Errorf("vendorVerb(%q, [setup]) stdout = %q, want cmdVendorSetup's report row", notAVendor, stdout)
	}
}

// TestVendorVerbRoutesResumeToCmdVendorResume: `resume` with no id hits cmdVendorResume's own usage
// check before ever reaching cmdAsk (which would otherwise dispatch a real agent run).
func TestVendorVerbRoutesResumeToCmdVendorResume(t *testing.T) {
	_, _, stderr := hosttest.CaptureOutput(t, func() int { return agentVerb("codex", []string{"resume"}) })
	if !strings.Contains(stderr, "codex resume: usage:") {
		t.Errorf("vendorVerb(resume, no id) stderr = %q, want cmdVendorResume's usage message", stderr)
	}
}

// seedTailJob saves a finished codex record whose log holds body and returns its id.
func seedTailJob(t *testing.T, status, body string) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cwd := t.TempDir()
	logPath := filepath.Join(cwd, "job.log")
	if err := os.WriteFile(logPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := job.NewRecord("codex", agent.RunOptions{Prompt: "tail me"}, time.Now())
	rec.Repo, rec.Status, rec.Log = cwd, status, logPath
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	return rec.ID
}

// TestVendorTailPrintsFinishedLog: tail <id> on a finished ok job prints its log and exits 0.
func TestVendorTailPrintsFinishedLog(t *testing.T) {
	id := seedTailJob(t, "ok", "line one\nline two\n")
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return agentVerb("codex", []string{"tail", id}) })
	if code != 0 || stdout != "line one\nline two\n" {
		t.Errorf("tail ok job = exit %d stdout %q stderr %q, want exit 0 with the log verbatim", code, stdout, stderr)
	}
}

// TestVendorTailErrorJobExitsOne: a job that ended in error still prints its log but exits 1.
func TestVendorTailErrorJobExitsOne(t *testing.T) {
	id := seedTailJob(t, "error", "boom\n")
	code, stdout, _ := hosttest.CaptureOutput(t, func() int { return agentVerb("codex", []string{"tail", id}) })
	if code != 1 || !strings.Contains(stdout, "boom") {
		t.Errorf("tail error job = exit %d stdout %q, want exit 1 with the log", code, stdout)
	}
}

// TestVendorTailUnknownIDFails: an id matching no job errors with the "<v> tail:" prefix, exit 1.
func TestVendorTailUnknownIDFails(t *testing.T) {
	seedTailJob(t, "ok", "x\n")
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return agentVerb("codex", []string{"tail", "zzzzzzzz"}) })
	if code != 1 || !strings.Contains(stderr, "codex tail:") {
		t.Errorf("tail unknown id = exit %d stderr %q, want exit 1 and the \"codex tail:\" prefix", code, stderr)
	}
}
