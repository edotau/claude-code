package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/audit"
)

// doctor --audit (text) reproduces exactly what the old `audit` verb printed for the same fixture
// log: one line per event, newest first, in "<time>  <action><pad><resource><pad><detail>" form.
func TestDoctorAuditTextMatchesOldAuditFormat(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	audit.Log(audit.ProviderSwitch, "gemini", "")
	audit.Log(audit.HookBlocked, "safety", "destructive command")

	code, stdout, stderr := captureOutput(t, func() int { return cmdDoctor([]string{"--audit"}) })
	if code != 0 {
		t.Fatalf("doctor --audit = %d, want 0: %s", code, stderr)
	}

	ev, err := audit.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for _, e := range ev {
		want.WriteString(e.Time.Local().Format("2006-01-02 15:04"))
		want.WriteString("  ")
		want.WriteString(pad(string(e.Action), 16))
		want.WriteString(" ")
		want.WriteString(pad(e.Resource, 22))
		want.WriteString(" ")
		want.WriteString(e.Detail)
		want.WriteString("\n")
	}
	if stdout != want.String() {
		t.Errorf("doctor --audit text =\n%q\nwant\n%q", stdout, want.String())
	}
}

// doctor --audit --format json carries the same Days/Summary/Events shape the old `audit --format
// json` printed.
func TestDoctorAuditJSONShape(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	audit.Log(audit.SecretSet, "OPENAI_API_KEY", "")

	code, stdout, stderr := captureOutput(t, func() int { return cmdDoctor([]string{"--audit", "--format", "json"}) })
	if code != 0 {
		t.Fatalf("doctor --audit --format json = %d, want 0: %s", code, stderr)
	}
	var got struct {
		Days    int                  `json:"days"`
		Summary map[audit.Action]int `json:"summary"`
		Events  []audit.Event        `json:"events"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("doctor --audit --format json did not decode: %v\n%s", err, stdout)
	}
	if got.Days != 7 {
		t.Errorf("Days = %d, want 7 (default)", got.Days)
	}
	if got.Summary[audit.SecretSet] != 1 || len(got.Events) != 1 {
		t.Errorf("got = %+v, want one SecretSet event", got)
	}
}

// doctor --audit --days rejects a non-positive value, same as the old `audit --days`.
func TestDoctorAuditRejectsNonPositiveDays(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := captureOutput(t, func() int { return cmdDoctor([]string{"--audit", "--days", "0"}) })
	if code != 2 {
		t.Errorf("doctor --audit --days 0 = %d, want 2", code)
	}
	if !strings.Contains(stderr, "usage: claude-code doctor --audit") {
		t.Errorf("stderr = %q, want the doctor --audit usage line", stderr)
	}
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}
