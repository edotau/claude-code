package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/claude"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/network/browser"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secretstore"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Every removed flag is a parse error carrying its hint; --profile stays valid.
func TestParseLaunchArgsRemovedFlags(t *testing.T) {
	for flag, want := range map[string]string{
		"--provider": "--profile", "--env": "--profile", "--tier": "--model", "--router": "removed",
	} {
		for _, claude := range []bool{true, false} {
			if _, _, err := parseLaunchArgs([]string{flag, "x"}, claude); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("parseLaunchArgs(%s, claude=%v) = %v, want hint %q", flag, claude, err, want)
			}
		}
	}
	o, rest, err := parseLaunchArgs([]string{"--profile", "openai", "-p", "hi"}, true)
	if err != nil || o.Provider != "openai" || !slices.Equal(rest, []string{"-p", "hi"}) {
		t.Errorf("--profile control: %+v %q %v", o, rest, err)
	}
}

func TestWarnVersion(t *testing.T) {
	setup := func(version string, tty bool) (string, *bytes.Buffer) {
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
		bin := pathstest.FakeVendor(t, "opencode", "echo 'opencode "+version+"'")
		var buf bytes.Buffer
		oldTTY, oldOut := agentVersionTTY, versionWarnOut
		agentVersionTTY, versionWarnOut = func() bool { return tty }, &buf
		t.Cleanup(func() { agentVersionTTY, versionWarnOut = oldTTY, oldOut })
		return bin, &buf
	}

	bin, buf := setup("2.0.0", true)
	warnVersion("opencode", bin, agent.LaunchOptions{}, nil)
	if got := buf.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "opencode 2.0.0 above 1.x") {
		t.Errorf("interactive launch warning = %q, want exactly one line naming `opencode 2.0.0 above 1.x`", got)
	}

	for name, tc := range map[string]struct {
		args []string
		o    agent.LaunchOptions
		tty  bool
		ver  string
	}{
		"--version":    {[]string{"--version"}, agent.LaunchOptions{}, true, "2.0.0"},
		"run":          {[]string{"run", "x"}, agent.LaunchOptions{}, true, "2.0.0"},
		"login":        {[]string{"auth", "login"}, agent.LaunchOptions{}, true, "2.0.0"},
		"print-env":    {nil, agent.LaunchOptions{PrintEnv: true}, true, "2.0.0"},
		"non-terminal": {nil, agent.LaunchOptions{}, false, "2.0.0"},
		"in bounds":    {nil, agent.LaunchOptions{}, true, "1.5.0"},
	} {
		t.Run(name, func(t *testing.T) {
			bin, buf := setup(tc.ver, tc.tty)
			warnVersion("opencode", bin, tc.o, tc.args)
			if buf.Len() != 0 {
				t.Errorf("warned %q, want silence", buf.String())
			}
		})
	}
}

const cliFakeToken = "sk-ant-oat01-FAKEFAKEFAKEFAKEFAKEFAKE0123456789"

func TestAnthropicSecretMissingOffTTYNamesVaultSet(t *testing.T) {
	useVaultTest(t)
	t.Setenv("MISSING_KEY_FOR_TEST", "")
	for _, mode := range []string{claude.AuthOAuth, claude.AuthAPIKey} {
		o := claude.AnthropicOptions{Mode: mode, Name: "MISSING_KEY_FOR_TEST"}
		_, err := anthropicSecret(o) // go test has no TTY on stdin: must error, never prompt
		if err == nil || !strings.Contains(err.Error(), "claude-code vault set MISSING_KEY_FOR_TEST") {
			t.Errorf("%s: err = %v", mode, err)
		}
	}
	o := claude.AnthropicOptions{Mode: claude.AuthOAuth, Name: "MISSING_KEY_FOR_TEST", PrintEnv: true}
	if _, err := anthropicSecret(o); err == nil || !strings.Contains(err.Error(), "claude-code vault set MISSING_KEY_FOR_TEST") {
		t.Errorf("--print-env: err = %v", err)
	}
}

func TestAnthropicSecretReadsVault(t *testing.T) {
	useVaultTest(t)
	vaulttest.Secret(t, "WORK_KEY", cliFakeToken)
	o := claude.AnthropicOptions{Mode: claude.AuthOAuth, Name: "WORK_KEY"}
	if v, err := anthropicSecret(o); err != nil || v != cliFakeToken {
		t.Errorf("oauth: %q %v", v, err)
	}
	o.Mode = claude.AuthAPIKey
	if v, err := anthropicSecret(o); err != nil || v != cliFakeToken {
		t.Errorf("api key: %q %v", v, err)
	}
}

func TestCmdAnthropicRefusalsDoNotEchoSecrets(t *testing.T) {
	useVaultTest(t)
	cases := [][]string{
		{"--oauth=" + cliFakeToken},
		{"--x-api-key=" + cliFakeToken},
		{"--oauth", "--x-api-key"},
		{"--profile", "anthropic"},
		{"--oauth=MISSING_KEY_FOR_TEST", "--print-env"},
	}
	for _, args := range cases {
		code, out, errOut := captureOutput(t, func() int { return cmdAnthropic(args) })
		if code == 0 {
			t.Errorf("%q: exit 0", args)
		}
		if strings.Contains(out+errOut, "FAKEFAKE") {
			t.Errorf("%q echoed the secret: %q", args, out+errOut)
		}
	}
}

func TestAnthropicHelpMentionsFlags(t *testing.T) {
	var b strings.Builder
	anthropicHelp(&b)
	for _, want := range []string{"--oauth", "--x-api-key", "--model"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("help lacks %s:\n%s", want, b.String())
		}
	}
}

// A validated env-shaped name may appear in the "not in the vault" error; PrintEnv avoids any prompt.
func TestAnthropicSecretMissingNamesKey(t *testing.T) {
	vaulttest.Use(t)
	const name = "NOPE_ABSENT_KEY_XYZ"
	t.Setenv(name, "")
	o := claude.AnthropicOptions{Mode: claude.AuthOAuth, Name: name, PrintEnv: true}
	_, err := anthropicSecret(o)
	if err == nil || !strings.Contains(err.Error(), name+" is not in the vault") {
		t.Fatalf("err = %v", err)
	}
}

func TestTakeAsBeatsProfileIdentity(t *testing.T) {
	as, rest, err := takeAs([]string{"--model", "x", "--as", "sp", "--", "--as", "user"})
	if err != nil || as != providers.IdentitySP {
		t.Fatalf("as=%q err=%v", as, err)
	}
	if len(rest) != 5 || rest[3] != "--as" {
		t.Fatalf("rest=%v", rest)
	}
	// the flag wins over a profile set to user
	if got, _ := providers.ResolveIdentity(providers.IdentityUser, as); got != providers.IdentitySP {
		t.Fatalf("resolved %q", got)
	}
	if _, _, err := takeAs([]string{"--as=bogus"}); err == nil {
		t.Fatal("bogus accepted")
	}
	if err := askAs("sp", "codex"); err == nil {
		t.Fatal("--as accepted for codex")
	}
}

func doctorGitFixture(t *testing.T, dirty bool) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) { t.Helper(); gitRun(t, dir, args...) }
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
				if err := secretstore.Store("AUDIT_FAKE_KEY", value); err != nil {
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
	t.Cleanup(browser.SetCommandTimeout(50 * time.Millisecond))
	if err := secretstore.Store("PLAIN_DOCTOR_FAKE_KEY", "obviously-fake-doctor-value"); err != nil {
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

// TestDoctorJSONWriteError ensures doctor --format json exits non-zero
// when writing to stdout fails (regression test for the doctor JSON write).
func TestDoctorJSONWriteError(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	orig := doctorChecks
	doctorChecks = func() ([]launch.Check, bool) { return []launch.Check{{Name: "x", OK: true}}, true }
	t.Cleanup(func() { doctorChecks = orig })
	closedStdout(t)

	code := cmdDoctor([]string{"--format", "json"})
	if code == 0 {
		t.Errorf("doctor --format json on closed stdout: exit %d, want non-zero", code)
	}
}

// TestDoctorAuditJSONWriteError ensures doctor --audit --format json exits non-zero
// when writing to stdout fails (regression test for the doctor audit JSON write).
func TestDoctorAuditJSONWriteError(t *testing.T) {
	closedStdout(t)

	code := cmdDoctor([]string{"--audit", "--format", "json"})
	if code == 0 {
		t.Errorf("doctor --audit --format json on closed stdout: exit %d, want non-zero", code)
	}
}

// A launch refusal (*agent.UsageError, even wrapped) prints its message and exits 2, not the generic 1.
func TestRunPlanUsageErrorExitsTwo(t *testing.T) {
	err := fmt.Errorf("launch: %w", &agent.UsageError{Msg: "not a harness profile"})
	if code := runPlan("claude", agent.Plan{}, agent.LaunchOptions{}, err); code != 2 {
		t.Fatalf("runPlan(UsageError) = %d, want 2", code)
	}
}
