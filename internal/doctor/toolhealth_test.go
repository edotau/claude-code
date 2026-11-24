package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/rtkwrap"
	"github.com/edotau/claude-code/internal/settings"
)

// Given host tools present or missing, When doctor renders them, Then a missing optional one is ⚠ and a required one ✗.
func TestDepDoctorDetail(t *testing.T) {
	for _, tc := range []struct {
		s        tools.DepStatus
		wantPass bool
		want     string
		mark     string
	}{
		{tools.DepStatus{Name: "shfmt", Path: "/b/shfmt", Version: "v3.14.1"}, true, "/b/shfmt v3.14.1", "✓"},
		{tools.DepStatus{Name: "tmux", Path: "/b/tmux"}, true, "/b/tmux", "✓"},
		{tools.DepStatus{Name: "ruff", Class: "feature", Use: "format-stop on .py files", Fix: "brew install ruff"},
			false, "warn missing (feature: format-stop on .py files); install: brew install ruff", "⚠"},
		{tools.DepStatus{Name: "git", Class: "required", Use: "worktrees", Fix: "xcode-select --install", Required: true},
			false, "missing (required: worktrees); install: xcode-select --install", "✗"},
	} {
		pass, got := depDoctorDetail(tc.s)
		if pass != tc.wantPass || got != tc.want {
			t.Errorf("depDoctorDetail(%s) = %v %q, want %v %q", tc.s.Name, pass, got, tc.wantPass, tc.want)
		}
		var b strings.Builder
		printCheckLines(&b, []check{{Name: "dep " + tc.s.Name, OK: pass, Detail: got}})
		if !strings.HasPrefix(b.String(), tc.mark) {
			t.Errorf("row for %s = %q, want mark %s", tc.s.Name, b.String(), tc.mark)
		}
	}
}

// Given the host-tool table, When doctor's tool checks run, Then every host tool gets one "dep <name>" row.
func TestCheckToolHealthListsDeps(t *testing.T) {
	rows := map[string]string{}
	checkToolHealth(context.Background(), func(_, _ bool, name, detail string) {
		if strings.HasPrefix(name, "dep ") {
			rows[name] = detail
		}
	})
	all := tools.DepStatusAll()
	if len(rows) != len(all) {
		t.Fatalf("doctor has %d dep rows, want %d: %v", len(rows), len(all), rows)
	}
	for _, s := range all {
		if _, ok := rows["dep "+s.Name]; !ok {
			t.Errorf("doctor has no row for %s", s.Name)
		}
	}
}

func TestRTKHookRows(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(rtkwrap.CodemodeEnv, "")
	t.Setenv("PATH", t.TempDir())
	if got := rtkAgentRow("gemini"); !got.OK || got.Detail != "awareness text only" {
		t.Errorf("Gemini without rtk binary = %+v", got)
	}
	installDoctorRTKPin(t)
	want := map[string]string{
		"rtk hook claude":   "warn rtk-rewrite hook not installed",
		"rtk hook codex":    "warn rtk-rewrite hook not installed",
		"rtk hook copilot":  "rewrite (SDK tool)",
		"rtk hook gemini":   "awareness text only",
		"rtk hook opencode": "rewrite (hook)",
		"rtk hook pi":       "warn shims not installed",
		"rtk PATH":          "warn rtk not on PATH (pinned binary may still work)",
	}
	rows := rtkHookRows()
	if len(rows) != len(want) {
		t.Fatalf("rtk rows = %+v, want %d rows", rows, len(want))
	}
	for _, row := range rows {
		if row.Detail != want[row.Name] {
			t.Errorf("%s = %q, want %q", row.Name, row.Detail, want[row.Name])
		}
	}

	t.Setenv(rtkwrap.CodemodeEnv, "on")
	for _, row := range rtkHookRows() {
		if row.Name != "rtk PATH" && row.Detail != "off in code mode (CODEMODE)" {
			t.Errorf("%s in code mode = %q", row.Name, row.Detail)
		}
	}
}

func TestRTKHookRowsInstalledHooks(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv(rtkwrap.CodemodeEnv, "")
	installDoctorRTKPin(t)
	claude := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"` +
		settings.DispatcherCommand(settings.EventPreToolUse) + `"}]}]}}`
	if err := os.WriteFile(settings.SettingsFile(), []byte(claude), 0o600); err != nil {
		t.Fatal(err)
	}
	command := (settings.EventSpec{Event: settings.EventPreToolUse, Name: "rtk-rewrite"}).Command()
	codex := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"` +
		settings.AgentEnv + `=codex ` + command + `"}]}]}}`
	if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "hooks.json"), []byte(codex), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := rtkAgentRow("claude"); !got.OK || got.Detail != "rewrite (hook)" {
		t.Errorf("installed Claude hook = %+v", got)
	}
	if got := rtkAgentRow("codex"); got.OK || got.Detail != "warn rtk-rewrite hook not installed" {
		t.Errorf("untrusted Codex hook = %+v", got)
	}
}

func installDoctorRTKPin(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(paths.BinDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.RTKPinned(), []byte("rtk"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestRTKHookRowsInstalledPiShims(t *testing.T) {
	installDoctorPiShims(t)
	installDoctorRTKPin(t)
	if got := rtkHookRows()[5]; !got.OK || got.Detail != "shims" {
		t.Errorf("installed Pi shim = %+v", got)
	}
	if err := os.Remove(filepath.Join(rtkwrap.Dir(), "git")); err != nil {
		t.Fatal(err)
	}
	if got := rtkHookRows()[5]; got.OK {
		t.Errorf("incomplete Pi shims = %+v", got)
	}
}

func installDoctorPiShims(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(rtkwrap.CodemodeEnv, "")
	if err := os.MkdirAll(rtkwrap.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.BinDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rtkwrap.Binary(), []byte("shim"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rtkwrap.Dir(), ".bash_env"), []byte("trap : EXIT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range append([]string{"rtk"}, rtkwrap.Names...) {
		if err := os.Symlink(rtkwrap.Binary(), filepath.Join(rtkwrap.Dir(), name)); err != nil {
			t.Fatal(err)
		}
	}
}

// Given codex's help lacks one declared flag, When doctor runs its tool checks, Then the row names it and the version.
func TestCheckToolHealthReportsFlagDrift(t *testing.T) {
	hermetic(t)
	help, err := os.ReadFile("testdata/codex-exec-help.txt")
	if err != nil {
		t.Fatal(err)
	}
	orig := agentHelp
	t.Cleanup(func() { agentHelp = orig })
	text := string(help)
	agentHelp = func(_ context.Context, spec agent.Schema) (string, string, error) {
		if spec.Name != "codex" {
			return "", "", tools.ErrAgentNotInstalled
		}
		return "0.166.0", text, nil
	}
	row := func() (bool, string) {
		var got *check
		checkToolHealth(context.Background(), func(ok, hard bool, name, d string) {
			if name == "agent flags codex" {
				got = &check{Name: name, OK: ok, Hard: hard, Detail: d}
			}
		})
		if got == nil {
			t.Fatal("no flag row for codex")
		}
		return got.OK, got.Detail
	}
	if ok, got := row(); !ok || got != "ok" {
		t.Errorf("full help = %v %q, want ok", ok, got)
	}
	text = strings.ReplaceAll(text, "--ephemeral", "--ephemeral-run")
	if ok, got := row(); ok || got != "drift: --ephemeral (installed 0.166.0)" {
		t.Errorf("help without --ephemeral = %v %q, want drift naming it (a longer flag does not count)", ok, got)
	}
	agentHelp = func(context.Context, agent.Schema) (string, string, error) {
		return "", "", errors.New("no help output")
	}
	if _, got := row(); got != "skipped: help unavailable" {
		t.Errorf("failed help = %q, want skipped", got)
	}
	checkExit1Help(t, row)
}

// checkExit1Help swaps agentHelp for a probe that exits 1: only the output with a flag reads as help.
func checkExit1Help(t *testing.T, row func() (bool, string)) {
	t.Helper()
	exit1 := errors.New("exit status 1")
	for _, out := range []string{"Options:\n  --other  x\n", "no flags here\n"} {
		agentHelp = func(context.Context, agent.Schema) (string, string, error) {
			text, err := helpText([]byte(out), exit1)
			return "0.166.0", text, err
		}
		_, got := row()
		if usable := !strings.HasPrefix(got, "skipped"); usable != strings.Contains(out, "--other") {
			t.Errorf("exit-1 help %q = %q, want drift only for the one with a flag", out, got)
		}
	}
}
