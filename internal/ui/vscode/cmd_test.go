package vscode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestThemeVSCodeServeNoLongerRoutes(t *testing.T) {
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return Run([]string{"serve"}) })
	if code != 1 {
		t.Errorf("theme vscode serve: exit %d, want 1", code)
	}
	if !strings.Contains(stderr, `unexpected argument "serve"`) {
		t.Errorf("stderr = %q", stderr)
	}
}

// isolated keeps sync calls off the real machine's keybindings.json and extensions dir.
func isolated(t *testing.T, tmpl, dest string) syncOptions {
	t.Helper()
	return syncOptions{Template: tmpl, Dest: dest, KeybindingsDest: filepath.Join(t.TempDir(), "keybindings.json"),
		ExtensionsDir: filepath.Join(t.TempDir(), "extensions")}
}

func writeVSCodeFixture(t *testing.T) (tmpl, dest string) {
	t.Helper()
	src := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "vscode", "claude-code-agents")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "package.json"), []byte(`{"publisher":"edotau","name":"claude-code-agents","version":"0.1.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tmpl = filepath.Join(dir, "template.json")
	dest = filepath.Join(dir, "settings.json")
	if err := os.WriteFile(tmpl, []byte(`{"chatgpt.cliExecutable": "__CLAUDE_BIN__/codex"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte(`{"local.only": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return tmpl, dest
}

// An explicit Dest with no KeybindingsDest defaults next to it, never to the real keybindings.json (shipped bug).
func TestCmdVSCodeDefaultsKeybindingsNextToExplicitDest(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // backups go to StateDir(); keep them out of the live tree
	tmpl, dest := writeVSCodeFixture(t)
	code, out, _ := hosttest.CaptureOutput(t, func() int {
		return vscodeSync(syncOptions{Template: tmpl, Dest: dest, ExtensionsDir: filepath.Join(t.TempDir(), "extensions")}, false, false)
	})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	wantKbDest := filepath.Join(filepath.Dir(dest), "keybindings.json")
	if !strings.Contains(out, wantKbDest) {
		t.Errorf("keybindings dest not defaulted beside %s:\n%s", dest, out)
	}
	if _, err := os.Stat(wantKbDest); err != nil {
		t.Errorf("keybindings.json not written beside the explicit Dest: %v", err)
	}
}

func TestCmdVSCodeMergesAndReportsBackup(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tmpl, dest := writeVSCodeFixture(t)
	code, out, _ := hosttest.CaptureOutput(t, func() int {
		return vscodeSync(isolated(t, tmpl, dest), false, false)
	})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "merged template") || !strings.Contains(out, "backup") {
		t.Errorf("missing merge/backup message: %q", out)
	}
	body, _ := os.ReadFile(dest)
	if !strings.Contains(string(body), `"local.only": true`) {
		t.Errorf("merge dropped the foreign key:\n%s", body)
	}
}

func TestCmdVSCodeDryRunWritesNothing(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tmpl, dest := writeVSCodeFixture(t)
	original, _ := os.ReadFile(dest)
	code, out, _ := hosttest.CaptureOutput(t, func() int {
		return vscodeSync(isolated(t, tmpl, dest), false, true)
	})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "would merge") {
		t.Errorf("missing dry-run message: %q", out)
	}
	after, _ := os.ReadFile(dest)
	if string(original) != string(after) {
		t.Error("--dry-run rewrote the destination")
	}
	if _, err := os.Stat(dest + ".bak-"); err == nil {
		t.Error("--dry-run left a backup file")
	}
}

func TestCmdVSCodeCheckExitsOneOnDrift(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tmpl, dest := writeVSCodeFixture(t)
	code, _, errOut := hosttest.CaptureOutput(t, func() int {
		return vscodeSync(isolated(t, tmpl, dest), true, false)
	})
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errOut, "DRIFT") {
		t.Errorf("missing DRIFT message: %q", errOut)
	}
	if body, _ := os.ReadFile(dest); !strings.Contains(string(body), `"local.only": true`) {
		t.Error("--check must not write")
	}
}

func TestCmdVSCodeCheckExitsZeroWhenInSync(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tmpl, dest := writeVSCodeFixture(t)
	o := isolated(t, tmpl, dest)
	if code, _, _ := hosttest.CaptureOutput(t, func() int {
		return vscodeSync(o, false, false)
	}); code != 0 {
		t.Fatal("initial merge failed")
	}
	code, out, _ := hosttest.CaptureOutput(t, func() int {
		return vscodeSync(o, true, false)
	})
	if code != 0 {
		t.Fatalf("exit %d, want 0 once merged", code)
	}
	if !strings.Contains(out, "in sync") {
		t.Errorf("missing in-sync message: %q", out)
	}
}

func TestCmdVSCodeCheckAndDryRunAreMutuallyExclusive(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tmpl, dest := writeVSCodeFixture(t)
	code, _, errOut := hosttest.CaptureOutput(t, func() int {
		return vscodeSync(isolated(t, tmpl, dest), true, true)
	})
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(errOut, "mutually exclusive") {
		t.Errorf("missing explanation: %q", errOut)
	}
}

func TestCmdVSCodeHelpExitsZeroWithoutRunning(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	_, dest := writeVSCodeFixture(t)
	original, _ := os.ReadFile(dest)
	for _, flagArg := range []string{"-h", "--help"} {
		code, _, errOut := hosttest.CaptureOutput(t, func() int {
			return Run([]string{flagArg})
		})
		if code != 0 {
			t.Errorf("vscode %s exit %d, want 0", flagArg, code)
		}
		if !strings.Contains(errOut, "User scope") || !strings.Contains(errOut, "Machine scope, which Settings Sync skips") {
			t.Errorf("vscode %s help missing scope note: %q", flagArg, errOut)
		}
	}
	if after, _ := os.ReadFile(dest); string(original) != string(after) {
		t.Error("-h/--help must not write the destination")
	}
}

// The real tracked template drives any fresh box's VS Code Machine settings, so its shape matters.
func TestTemplateIsFirstPartyOnly(t *testing.T) {
	tmpl, err := os.ReadFile("machine-settings.template.json")
	if err != nil {
		t.Fatal(err)
	}
	body := string(tmpl)
	for _, want := range []string{
		`"chatgpt.cliExecutable": "__CLAUDE_BIN__/codex"`,
		`"chat.useClaudeHooks"`,
		`"github.copilot.`,
		`"terminal.integrated.`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("template missing %q", want)
		}
	}
	for _, unwanted := range []string{`"python.`, `"[python]"`, "databricks", "Databricks"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("template retained unwanted key %q", unwanted)
		}
	}
}
