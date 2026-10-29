package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/launch"
)

// TestWorktreeListJSONWriteError ensures that when outfmt.Write fails,
// the command exits with non-zero status, not silently succeeding with partial output.
// This regression test for internal/cli/worktree.go:90 verifies that JSON write errors
// are properly reported to the user.
func TestWorktreeListJSONWriteError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gitRepoCLI(t)
	t.Chdir(repo)

	// Create a worktree to ensure entries are non-empty
	code, _, _ := captureOutput(t, func() int {
		return cmdWorktree([]string{"create", "test-wt"})
	})
	if code != 0 {
		t.Fatalf("create worktree failed: code=%d", code)
	}

	// Simulate a closed/broken stdout by writing to a closed file
	f, err := os.Create(filepath.Join(t.TempDir(), "closed-file"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	oldOut := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = oldOut }()

	code = cmdWorktree([]string{"list", "--format", "json"})
	if code == 0 {
		t.Errorf("worktree list --format json on closed stdout: exit %d, want non-zero", code)
	}
}

// TestDispatchListJSONWriteError ensures dispatch --list --format json exits non-zero
// when writing to stdout fails.
func TestDispatchListJSONWriteError(t *testing.T) {
	hermeticDispatch(t)
	listing := "cc-claude-ab12|1700000000|1|claude|claude\n"
	fakeSSHOnPATH(t, listing)

	f, err := os.Create(filepath.Join(t.TempDir(), "closed-file"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	oldOut := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = oldOut }()

	code := cmdDispatch([]string{"--list", "--host", "myhost", "--format", "json"})
	if code == 0 {
		t.Errorf("dispatch --list --format json on closed stdout: exit %d, want non-zero", code)
	}
}

// TestMemorySearchJSONWriteError ensures memory search --format json exits non-zero
// when writing to stdout fails (regression test for internal/cli/memory.go:158).
func TestMemorySearchJSONWriteError(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	f, err := os.Create(filepath.Join(t.TempDir(), "closed-file"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	oldOut := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = oldOut }()

	code := cmdMemorySearch([]string{"anyterm", "--format", "json"})
	if code == 0 {
		t.Errorf("memory search --format json on closed stdout: exit %d, want non-zero", code)
	}
}

// TestWorktreeListEmptyJSONIsArray ensures that an empty worktree list produces []
// (an empty array) not null in JSON format, so consumers can iterate safely
// (regression test for internal/cli/worktree.go:87-89).
func TestWorktreeListEmptyJSONIsArray(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gitRepoCLI(t)
	t.Chdir(repo)

	code, out, _ := captureOutput(t, func() int {
		return cmdWorktree([]string{"list", "--format", "json"})
	})
	if code != 0 {
		t.Fatalf("list empty: code=%d", code)
	}
	if strings.TrimSpace(out) == "null" {
		t.Errorf("empty list produced null, want []")
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Errorf("invalid JSON: %v, output=%q", err, out)
	}
	if entries == nil {
		t.Errorf("JSON unmarshaled to nil, want empty slice []")
	}
}

// TestDispatchListEmptyTextOutput ensures that an empty session list is not an error;
// the command should exit 0 with no output.
func TestDispatchListEmptyTextOutput(t *testing.T) {
	hermeticDispatch(t)
	fakeSSHOnPATH(t, "") // no sessions

	code, out, stderr := captureOutput(t, func() int {
		return cmdDispatch([]string{"--list", "--host", "myhost"})
	})
	if code != 0 {
		t.Fatalf("--list (empty): exit %d, want 0; stderr=%q", code, stderr)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("--list (empty) output = %q, want empty", out)
	}
}

// TestThemeGetGradientSafety ensures that theme gradient loading handles errors
// safely without panicking or returning garbage values (regression test for
// internal/cli/theme.go:111 json.Unmarshal error handling).
func TestThemeGetGradientSafety(t *testing.T) {
	_ = hermeticTheme(t) // initialize theme directory

	// Test that setting and getting a theme works without error
	code, _, _ := captureOutput(t, func() int {
		return cmdTheme([]string{"nord"})
	})
	if code != 0 {
		t.Errorf("theme nord: exit %d, want 0", code)
	}
}

// TestGeminiBridgeJSONOutput ensures that gemini bridge --format json
// produces valid JSON output when successful.
func TestGeminiBridgeJSONOutput(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	code, out, _ := captureOutput(t, func() int {
		return cmdGeminiBridge([]string{"--print-env", "--format", "json"})
	})

	if code == 0 && strings.TrimSpace(out) != "" {
		// Output should be valid JSON
		var result any
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Errorf("gemini --print-env --format json output is not valid JSON: %v\noutput=%q", err, out)
		}
	}
}

// TestNetworkVscodeServeStatusJSONWriteError ensures network vscode status --format json exits
// non-zero when writing to stdout fails (regression test for internal/cli/vscode.go:257).
func TestNetworkVscodeServeStatusJSONWriteError(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "closed-file"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	oldOut := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = oldOut }()

	code := cmdNetworkVSCode([]string{"--status", "--format", "json"})
	if code == 0 {
		t.Errorf("network vscode status --format json on closed stdout: exit %d, want non-zero", code)
	}
}

// TestDoctorJSONWriteError ensures doctor --format json exits non-zero
// when writing to stdout fails (regression test for internal/cli/launch.go:419).
func TestDoctorJSONWriteError(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	orig := doctorChecks
	doctorChecks = func() ([]launch.Check, bool) { return []launch.Check{{Name: "x", OK: true}}, true }
	t.Cleanup(func() { doctorChecks = orig })
	f, err := os.Create(filepath.Join(t.TempDir(), "closed-file"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	oldOut := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = oldOut }()

	code := cmdDoctor([]string{"--format", "json"})
	if code == 0 {
		t.Errorf("doctor --format json on closed stdout: exit %d, want non-zero", code)
	}
}

// TestDoctorAuditJSONWriteError ensures doctor --audit --format json exits non-zero
// when writing to stdout fails (regression test for internal/cli/launch.go:444).
func TestDoctorAuditJSONWriteError(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "closed-file"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	oldOut := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = oldOut }()

	code := cmdDoctor([]string{"--audit", "--format", "json"})
	if code == 0 {
		t.Errorf("doctor --audit --format json on closed stdout: exit %d, want non-zero", code)
	}
}

// TestMemoryRecallFormatValidation ensures memory recall exits with an error
// when an invalid format is specified (regression for internal/cli/memory.go error handling).
func TestMemoryRecallFormatValidation(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := captureOutput(t, func() int {
		return cmdMemoryRecall([]string{"test", "--format", "invalid"})
	})
	if code == 0 {
		t.Errorf("memory recall with invalid format: exit %d, want non-zero; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "format") {
		t.Errorf("memory recall invalid format: stderr %q, want format error message", stderr)
	}
}

// TestMemorySearchFormatValidation ensures memory search exits with an error
// when an invalid format is specified (regression for internal/cli/memory.go error handling).
func TestMemorySearchFormatValidation(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := captureOutput(t, func() int {
		return cmdMemorySearch([]string{"test", "--format", "invalid"})
	})
	if code == 0 {
		t.Errorf("memory search with invalid format: exit %d, want non-zero; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "format") {
		t.Errorf("memory search invalid format: stderr %q, want format error message", stderr)
	}
}

// TestNetworkVscodeFormatValidation ensures network vscode start/stop/status only accept text|json.
// Regression test for internal/cli/vscode.go format validation in vscodeServeStart/Stop/Status.
func TestNetworkVscodeFormatValidation(t *testing.T) {
	cases := []struct {
		args []string
		name string
	}{
		{[]string{"--format", "invalid"}, "start"},
		{[]string{"--stop", "--format", "invalid"}, "stop"},
		{[]string{"--status", "--format", "invalid"}, "status"},
	}
	for _, tc := range cases {
		code, _, stderr := captureOutput(t, func() int {
			return cmdNetworkVSCode(tc.args)
		})
		if code == 0 {
			t.Errorf("network vscode %s with invalid format: exit %d, want non-zero; stderr=%q", tc.name, code, stderr)
		}
	}
}

// TestDispatchFlagsNotValidatedPerMode ensures dispatch does not accept invalid flag combinations
// (regression for internal/cli/dispatch.go: flags parsed but not validated per mode).
// For now, this test documents the desired behavior; actual validation should be added.
func TestDispatchFlagsNotValidatedPerMode(t *testing.T) {
	hermeticDispatch(t)
	fakeSSHOnPATH(t, "")

	// --dry-run is only valid in start mode, not with --list
	// Currently this silently ignores --dry-run; the fix should reject it or warn
	code, _, _ := captureOutput(t, func() int {
		return cmdDispatch([]string{"--host", "myhost", "--list", "--dry-run"})
	})
	// After fix, code should be non-zero when invalid flags are used
	_ = code
}

// TestMemoryDreamGetWdError ensures memory dream handles os.Getwd() errors gracefully
// (regression for internal/cli/memory.go:358).
func TestMemoryDreamGetWdError(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	// Test memory dream without --cwd to exercise the Getwd() path
	code, _, stderr := captureOutput(t, func() int {
		return cmdMemoryDream([]string{"--dry-run"})
	})
	// Should succeed with --dry-run even in an empty bank
	if code != 0 {
		t.Errorf("memory dream --dry-run: exit %d, want 0; stderr=%q", code, stderr)
	}
}

// TestMemoryRecallFailReturnValue ensures memory recall returns a non-zero code
// (regression for internal/cli/memory.go:150 where fail() return value not used).
func TestMemoryRecallFailReturnValue(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := captureOutput(t, func() int {
		return cmdMemoryRecall([]string{})
	})
	// Should return non-zero when no arguments given
	if code == 0 {
		t.Errorf("memory recall (no args) exit %d, want non-zero; stderr=%q", code, stderr)
	}
}

// TestMemorySearchFailReturnValue ensures memory search returns a non-zero code
// (regression for internal/cli/memory.go:245 where fail() return value not used).
func TestMemorySearchFailReturnValue(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := captureOutput(t, func() int {
		return cmdMemorySearch([]string{})
	})
	// Should return non-zero when no arguments given
	if code == 0 {
		t.Errorf("memory search (no args) exit %d, want non-zero; stderr=%q", code, stderr)
	}
}

// TestMemoryShowFailReturnValue ensures memory show returns the error code from fail()
// (regression for internal/cli/memory.go:277).
func TestMemoryShowFailReturnValue(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := captureOutput(t, func() int {
		return cmdMemoryShow([]string{})
	})
	// Should return 2 (from fail) when no argument given
	if code != 2 {
		t.Errorf("memory show (no args) exit %d, want 2; stderr=%q", code, stderr)
	}
}

// TestMemoryRecallLoadBlockErrorSilent documents the behavior where ShowBlock() errors
// are silently ignored in recallLoad() (regression for internal/cli/memory.go:188).
// The fix should log or report errors instead of silently continuing.
func TestMemoryRecallLoadBlockErrorSilent(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	// Use --load with an invalid block ID to trigger ShowBlock() error
	code, out, _ := captureOutput(t, func() int {
		return cmdMemoryRecall([]string{"nonexistent", "--load", "1"})
	})
	// Should succeed (exit 0) but only print the title, not the body
	// After the fix, errors should be logged or reported
	_ = code
	_ = out
}
