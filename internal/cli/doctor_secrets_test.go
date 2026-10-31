package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/secrets"
)

func doctorGitFixture(t *testing.T, dirty bool) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if err := cmd.Run(); err != nil {
			t.Fatalf("scratch git %s failed: %v", args[0], err)
		}
	}
	run("init", "-q")
	run("config", "user.email", "fake@example.invalid")
	run("config", "user.name", "fixture")
	text := "nothing sensitive\n"
	if dirty {
		text = "FAKE_TOKEN=" + "gh" + "p_obviouslyFAKEfixtureToken1234567890\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture.env"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "fixture.env")
	run("-c", "core.hooksPath=/dev/null", "commit", "-qm", "fixture")
	return dir
}

func TestDoctorSecretAuditExitCodes(t *testing.T) {
	for _, mode := range []string{"scan", "git", "secrets"} {
		for _, finding := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "/clean", true: "/finding"}[finding], func(t *testing.T) {
				cfg := useVaultTest(t)
				t.Setenv("HOME", t.TempDir())
				t.Chdir(doctorGitFixture(t, finding && mode != "scan"))
				const value = "gh" + "p_obviouslyFAKEfixtureToken1234567890"
				data := `{"env":{"FAKE_TOKEN":"${FAKE_TOKEN}"}}`
				if finding && mode != "git" {
					data = `{"env":{"FAKE_TOKEN":"` + value + `"}}`
				}
				path := filepath.Join(cfg, "settings.json")
				if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
				args := []string{"--" + mode}
				if mode == "scan" {
					args = append(args, path)
				} else {
					args = append(args, "--since", "1")
				}
				code, stdout, stderr := captureOutput(t, func() int { return cmdDoctor(args) })
				want := 0
				if finding {
					want = 1
				}
				if code != want {
					t.Errorf("doctor --%s exit %d, want %d", mode, code, want)
				}
				if strings.Contains(stdout+stderr, value) {
					t.Fatal("doctor leaked the fake raw value")
				}
				prefix := "doctor --scan:"
				if mode == "git" {
					prefix = "doctor --git:"
				}
				if !strings.Contains(stdout, prefix) {
					t.Errorf("missing audit prefix %q", prefix)
				}
			})
		}
	}
}

func TestDoctorSecretsWorstExitWins(t *testing.T) {
	for _, finding := range []string{"scan", "git", "vault"} {
		t.Run(finding, func(t *testing.T) {
			cfg := useVaultTest(t)
			t.Setenv("HOME", t.TempDir())
			t.Chdir(doctorGitFixture(t, finding == "git"))
			const value = "gh" + "p_obviouslyFAKEfixtureToken1234567890"
			data := "{}"
			if finding == "scan" {
				data = `{"env":{"FAKE_TOKEN":"` + value + `"}}`
			}
			if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if finding == "vault" {
				if err := secrets.Store("AUDIT_FAKE_KEY", value); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(filepath.Join(cfg, "state", "vault.db"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			code, stdout, stderr := captureOutput(t, func() int {
				return cmdDoctor([]string{"--secrets", "--since", "1"})
			})
			if code != 1 {
				t.Errorf("%s finding was lost: exit %d, want 1", finding, code)
			}
			if strings.Contains(stdout+stderr, value) {
				t.Fatal("combined doctor audit leaked a raw value")
			}
		})
	}
}

func TestDoctorAuditUsageErrors(t *testing.T) {
	useVaultTest(t)
	for _, args := range [][]string{
		{"unexpected"}, {"--git", "unexpected"},
		{"--scan", "--format", "json"}, {"--git", "--format", "json"},
		{"--secrets", "--format", "json"},
	} {
		code, _, _ := captureOutput(t, func() int { return cmdDoctor(args) })
		if code != 2 {
			t.Errorf("doctor %v exit %d, want 2", args, code)
		}
	}
}

func TestDoctorPlainIncludesVaultRows(t *testing.T) {
	useVaultTest(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no real tool --version probes
	if err := secrets.Store("PLAIN_DOCTOR_FAKE_KEY", "obviously-fake-doctor-value"); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"text", "json"} {
		code, stdout, _ := captureOutput(t, func() int { return cmdDoctor([]string{"--format", format}) })
		if strings.Contains(stdout, "obviously-fake-doctor-value") {
			t.Fatal("plain doctor leaked a raw value")
		}
		if code != 0 && code != 1 {
			t.Fatalf("doctor exit %d", code)
		}
		if format == "text" {
			for _, name := range []string{"vault", "vault perms", "vault key names", "keyring tool"} {
				if !strings.Contains(stdout, name) {
					t.Errorf("plain doctor missing %s", name)
				}
			}
			if n := strings.Count(stdout, "vault master key"); n != 0 {
				t.Errorf("plain doctor's \"vault\" row already carries the master-key status; want zero \"vault master key\" rows, got %d", n)
			}
			continue
		}
		var doc struct {
			Checks []struct {
				Name string `json:"name"`
			} `json:"checks"`
		}
		if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, c := range doc.Checks {
			names = append(names, c.Name)
		}
		for _, name := range []string{"vault", "vault perms", "vault key names", "keyring tool"} {
			if !slices.Contains(names, name) {
				t.Errorf("JSON doctor missing %s", name)
			}
		}
		n := 0
		for _, name := range names {
			if name == "vault master key" {
				n++
			}
		}
		if n != 0 {
			t.Errorf("JSON doctor's \"vault\" row already carries the master-key status; want zero \"vault master key\" rows, got %d", n)
		}
	}
}

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
	if !strings.Contains(stderr, "usage: claude-code doctor") || !strings.Contains(stderr, "--audit") {
		t.Errorf("stderr = %q, want the doctor usage and --audit flag", stderr)
	}
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}
