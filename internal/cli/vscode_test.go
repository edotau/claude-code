package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// kbDestFlag isolates every cmdVSCode call below from the real machine's keybindings.json: without
// it, a call that omits --keybindings-dest falls back to the platform default and writes there.
func kbDestFlag(t *testing.T) []string {
	t.Helper()
	return []string{"--keybindings-dest", filepath.Join(t.TempDir(), "keybindings.json")}
}

func writeVSCodeFixture(t *testing.T) (tmpl, dest string) {
	t.Helper()
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

// An explicit --dest with no --keybindings-dest must default next to that dest, never to the
// platform's real keybindings.json — the bug this guards shipped keybindings into the live machine.
func TestCmdVSCodeDefaultsKeybindingsNextToExplicitDest(t *testing.T) {
	tmpl, dest := writeVSCodeFixture(t)
	code, out, _ := captureOutput(t, func() int {
		return cmdVSCode([]string{"--template", tmpl, "--dest", dest})
	})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	wantKbDest := filepath.Join(filepath.Dir(dest), "keybindings.json")
	if !strings.Contains(out, wantKbDest) {
		t.Errorf("keybindings dest not defaulted beside %s:\n%s", dest, out)
	}
	if _, err := os.Stat(wantKbDest); err != nil {
		t.Errorf("keybindings.json not written beside the explicit --dest: %v", err)
	}
}

func TestCmdVSCodeMergesAndReportsBackup(t *testing.T) {
	tmpl, dest := writeVSCodeFixture(t)
	code, out, _ := captureOutput(t, func() int {
		return cmdVSCode(append([]string{"--template", tmpl, "--dest", dest}, kbDestFlag(t)...))
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
	tmpl, dest := writeVSCodeFixture(t)
	original, _ := os.ReadFile(dest)
	code, out, _ := captureOutput(t, func() int {
		return cmdVSCode(append([]string{"--template", tmpl, "--dest", dest, "--dry-run"}, kbDestFlag(t)...))
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
	tmpl, dest := writeVSCodeFixture(t)
	code, _, errOut := captureOutput(t, func() int {
		return cmdVSCode(append([]string{"--template", tmpl, "--dest", dest, "--check"}, kbDestFlag(t)...))
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
	tmpl, dest := writeVSCodeFixture(t)
	kbFlag := kbDestFlag(t)
	if code, _, _ := captureOutput(t, func() int {
		return cmdVSCode(append([]string{"--template", tmpl, "--dest", dest}, kbFlag...))
	}); code != 0 {
		t.Fatal("initial merge failed")
	}
	code, out, _ := captureOutput(t, func() int {
		return cmdVSCode(append([]string{"--template", tmpl, "--dest", dest, "--check"}, kbFlag...))
	})
	if code != 0 {
		t.Fatalf("exit %d, want 0 once merged", code)
	}
	if !strings.Contains(out, "in sync") {
		t.Errorf("missing in-sync message: %q", out)
	}
}

func TestCmdVSCodeCheckAndDryRunAreMutuallyExclusive(t *testing.T) {
	tmpl, dest := writeVSCodeFixture(t)
	code, _, errOut := captureOutput(t, func() int {
		return cmdVSCode(append([]string{"--template", tmpl, "--dest", dest, "--check", "--dry-run"}, kbDestFlag(t)...))
	})
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(errOut, "mutually exclusive") {
		t.Errorf("missing explanation: %q", errOut)
	}
}

func TestCmdVSCodeHelpExitsZeroWithoutRunning(t *testing.T) {
	tmpl, dest := writeVSCodeFixture(t)
	original, _ := os.ReadFile(dest)
	for _, flagArg := range []string{"-h", "--help"} {
		code, _, errOut := captureOutput(t, func() int {
			return cmdVSCode(append([]string{flagArg, "--template", tmpl, "--dest", dest}, kbDestFlag(t)...))
		})
		if code != 0 {
			t.Errorf("vscode %s exit %d, want 0", flagArg, code)
		}
		if !strings.Contains(errOut, "Machine") || !strings.Contains(errOut, "not Settings-Synced") {
			t.Errorf("vscode %s help missing Machine-scope note: %q", flagArg, errOut)
		}
	}
	if after, _ := os.ReadFile(dest); string(original) != string(after) {
		t.Error("-h/--help must not write the destination")
	}
}

// The real tracked template drives any fresh box's VS Code Machine settings, so its shape matters.
func TestTemplateIsFirstPartyOnly(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	tmpl, err := os.ReadFile(filepath.Join(root, "internal", "vscode", "machine-settings.template.json"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(tmpl)
	for _, want := range []string{
		`"claudeCode.claudeProcessWrapper": "__CLAUDE_BIN__/claude"`,
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
			t.Errorf("template retained non-first-party key %q", unwanted)
		}
	}
}
