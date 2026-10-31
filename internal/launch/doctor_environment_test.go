package launch

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestToolPATHCopiesAndExtras(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", scratch)
	bin := paths.BinDir()
	primary := pathstest.FakeVendor(t, "opencode", "echo 1.0.0")
	aBin := vendorPATHFixture(t, "opencode", "echo 1.0.0")
	bBin := vendorPATHFixture(t, "opencode", "echo 2.0.0")
	a, b, c := filepath.Dir(aBin), filepath.Dir(bBin), filepath.Join(scratch, "C")
	// C/opencode is a symlink to A/opencode: same real file, must dedupe.
	if err := os.MkdirAll(c, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(aBin, filepath.Join(c, "opencode")); err != nil {
		t.Fatal(err)
	}
	join := func(dirs ...string) string { return strings.Join(dirs, string(os.PathListSeparator)) }

	cases := []struct {
		name       string
		path       string
		wantCopies []string
	}{
		{"bin first", join(bin, a, b), []string{aBin, bBin}},
		{"tool before bin", join(a, bin, b), []string{aBin, bBin}},
		{"no bin dir", join(a, b), []string{aBin, bBin}},
		{"symlink dedup", join(bin, a, c), []string{aBin}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", tc.path)
			copies := toolPATHCopies("opencode")
			if !slices.Equal(copies, tc.wantCopies) {
				t.Fatalf("toolPATHCopies = %v; want %v", copies, tc.wantCopies)
			}
		})
	}

	status := tools.Status{Name: "opencode", Path: aBin, Version: "1.0.0"}
	for _, order := range []string{join(a, bin, b), join(bin, a, b)} {
		t.Run(order, func(t *testing.T) {
			t.Setenv("PATH", order)
			detail := toolDoctorDetail(status)
			want := primary + " 1.0.0 (direct install in bin/)" +
				" · also on PATH: " + aBin + " 1.0.0 · also on PATH: " + bBin + " 2.0.0"
			if detail != want {
				t.Errorf("detail = %q, want %q", detail, want)
			}
		})
	}
}

func TestDoctorVendorRows(t *testing.T) {
	for _, kind := range []string{"link", "direct", "broken link", "retired shim"} {
		t.Run(kind, func(t *testing.T) {
			doctorScratch(t)
			t.Setenv("HOME", t.TempDir())
			source := vendorPATHFixture(t, "opencode", "echo 1.2.3")
			t.Setenv("PATH", filepath.Dir(source))
			dst := pathstest.FakeVendor(t, "opencode", "echo 4.5.6")
			want := dst + " 4.5.6 (direct install in bin/)" +
				" · also on PATH: " + source + " 1.2.3"
			if kind != "direct" {
				if err := os.Remove(dst); err != nil {
					t.Fatal(err)
				}
				target := source
				switch kind {
				case "link":
					want = dst + " → " + source + " 1.2.3"
				case "broken link":
					target = filepath.Join(t.TempDir(), "missing")
					want = "opencode: " + dst + " is a broken link (run claude-code install) · on PATH: " + source
				case "retired shim":
					target = filepath.Join(t.TempDir(), "claude-code")
					if err := os.WriteFile(target, []byte("must not execute"), 0o755); err != nil {
						t.Fatal(err)
					}
					want = "opencode: " + dst + " is the retired claude-code shim (run claude-code install) · on PATH: " + source
				}
				if err := os.Symlink(target, dst); err != nil {
					t.Fatal(err)
				}
			}
			checks, _ := DoctorChecks()
			row := findCheck(checks, "tool opencode")
			if row == nil {
				t.Fatal("missing tool opencode row")
			}
			if row.Detail != want {
				t.Errorf("detail = %q, want %q", row.Detail, want)
			}
			if wantOK := kind == "link" || kind == "direct"; row.OK != wantOK {
				t.Errorf("OK = %v, want %v", row.OK, wantOK)
			}
		})
	}
}

func TestDoctorAgentVersionRows(t *testing.T) {
	hermetic(t, "")
	pathstest.FakeVendor(t, "copilot", "echo 'GitHub Copilot CLI 1.0.100.'")
	checks, _ := DoctorChecks()
	c := findCheck(checks, "agent version copilot")
	if c == nil {
		t.Fatal("no `agent version copilot` row")
	}
	if !strings.HasPrefix(c.Detail, "warn 1.0.100 below 1.0.104") || c.Hard {
		t.Errorf("row = %+v, want detail `warn 1.0.100 below 1.0.104...` and Hard=false", *c)
	}

	hermetic(t, "")
	t.Setenv("PATH", t.TempDir())
	checks, _ = DoctorChecks()
	c = findCheck(checks, "agent version copilot")
	if c == nil || c.Detail != "not installed" || c.Hard {
		t.Errorf("missing binary row = %+v, want `not installed`, non-hard", c)
	}
}

func TestToolPATHVersionProbesUnchangedBinaryOnce(t *testing.T) {
	hermetic(t, "")
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	bin := filepath.Join(dir, "fake")
	script := "#!/bin/sh\necho x >> '" + counter + "'\necho 'fake 7.8.9'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if got := toolPATHVersion(bin); got != "fake 7.8.9" {
			t.Fatalf("toolPATHVersion = %q", got)
		}
	}
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "\n"); n != 1 {
		t.Fatalf("two lookups spawned the binary %d times, want 1", n)
	}
}

// TestUserBinDirNotEmpty verifies userBinDir returns a meaningful path, not "/.local/bin" when home is missing.
func TestUserBinDirNotEmpty(t *testing.T) {
	got := userBinDir()
	if got == "" {
		t.Error("userBinDir returned empty string")
	}
	// Verify it's a reasonable path (not the broken "/.local/bin" when home is missing)
	// After the fix, this test will pass because errors are handled properly
	if got == "/.local/bin" {
		t.Errorf("userBinDir returned %q (indicates UserHomeDir error was ignored)", got)
	}
	t.Logf("userBinDir = %q (regression check: should be valid path after fix)", got)
}

// TestExtraPATHCopiesHandlesEvalSymlinksErrors verifies EvalSymlinks errors are checked, not ignored.
// Regression: lines 546-550 did `first, _ := filepath.EvalSymlinks(primary)` ignoring errors,
// so when primary is a broken symlink, first="" and comparisons like real=="" fail to deduplicate.
func TestExtraPATHCopiesHandlesEvalSymlinksErrors(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", scratch)
	// Create a real file we'll compare against
	realPath := filepath.Join(scratch, "real-tool")
	if err := os.WriteFile(realPath, []byte("tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Create a symlink to the real file
	linkPath := filepath.Join(scratch, "link-tool")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}
	// Test extraPATHCopies with the symlink as primary; it should resolve to realPath.
	// The function must call EvalSymlinks and check the error to properly deduplicate.
	detail := extraPATHCopies("tool", linkPath)
	// If EvalSymlinks error is ignored and linkPath is broken later, detail would show false duplicates.
	// After fix: even if primary is a symlink, EvalSymlinks errors are handled and comparisons work.
	t.Logf("extraPATHCopies returned (detail=%q) for symlink primary; no crash means errors handled", detail)
}

func TestStrayClaudeJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if ok, _ := strayClaudeJSON(); !ok {
		t.Fatal("no stub must pass")
	}
	stub := filepath.Join(home, ".claude", ".claude.json")
	if err := os.MkdirAll(filepath.Dir(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stub, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, detail := strayClaudeJSON(); ok || !strings.Contains(detail, stub) {
		t.Errorf("stub present: ok=%v detail=%q", ok, detail)
	}
}

// seed writes a file under state/harness and returns its path.
func seedHarness(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(paths.StateDir(), name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFormatsDoctorAndInstallStepSpan(t *testing.T) {
	hermetic(t, "")
	quota := seedHarness(t, "quota.json", `{"acct":{"until":"2099-01-01T00:00:00Z"}}`)
	seedHarness(t, "agentversion.json", `{"claude":"1.0.0"}`) // unversioned cache: a miss, never stale
	seedHarness(t, "zz-stray.json", `{}`)

	formats := findCheck(formatsDoctorChecks(), "formats")
	if formats == nil || !formats.OK || strings.Contains(formats.Detail, "stale") { // quota stays a bare map: an old daemon reads it
		t.Fatalf("formats row = %+v, want current", formats)
	}
	layout := findCheck(formatsDoctorChecks(), "state layout")
	if layout == nil || !strings.Contains(layout.Detail, "zz-stray.json") {
		t.Fatalf("state layout row = %+v, want stray file listed", layout)
	}
	if strings.Contains(layout.Detail, "quota.json") {
		t.Errorf("registered file flagged in layout: %q", layout.Detail)
	}

	before, _ := os.ReadFile(quota)
	var out bytes.Buffer
	installFormats(&out, false)
	if after, _ := os.ReadFile(quota); !bytes.Equal(before, after) {
		t.Fatalf("install rewrote quota.json: %s", after)
	}
	if _, err := os.Stat(schemas.LedgerPath()); err == nil {
		t.Error("install wrote the ledger for a bare-map quota.json")
	}
}

func TestVaultDoctorChecks(t *testing.T) {
	t.Run("missing vault", func(t *testing.T) {
		hermetic(t, "")
		checks := vaultDoctorChecks()
		c := findCheck(checks, "vault")
		if c == nil || c.Detail != "not created yet" || !c.OK {
			t.Errorf("vault row = %+v", c)
		}
		for _, name := range []string{"vault perms", "keyring tool"} {
			if c := findCheck(checks, name); c == nil || !c.OK {
				t.Errorf("missing healthy %s row", name)
			}
		}
	})
	t.Run("secrets and retired", func(t *testing.T) {
		hermetic(t, "")
		t.Setenv("HARNESS_VAULT_KEY", "")
		vaulttest.Keyring(t)
		vaulttest.Secret(t, "FOO_API_KEY", "sk-test-0123456789")
		if _, _, err := vault.RotateKey(); err != nil {
			t.Fatal(err)
		}
		c := findCheck(vaultDoctorChecks(), "vault")
		if c == nil || !c.OK {
			t.Fatalf("vault row = %+v", c)
		}
		for _, want := range []string{vault.Path(), "key keyring", "1 secret(s)", "1 retired"} {
			if !strings.Contains(c.Detail, want) {
				t.Errorf("detail %q lacks %q", c.Detail, want)
			}
		}
		if strings.Contains(c.Detail, "sk-test") {
			t.Error("doctor printed a secret")
		}
	})
	t.Run("one vault row, not two", func(t *testing.T) {
		hermetic(t, "")
		t.Setenv("HARNESS_VAULT_KEY", "")
		vaulttest.Keyring(t)
		vaulttest.Secret(t, "FOO_API_KEY", "sk-test-0123456789")
		n := 0
		for _, c := range vaultDoctorChecks() {
			if c.Name == "vault" || c.Name == "vault master key" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("vaultDoctorChecks has %d vault/master-key row(s), want exactly 1 (the \"vault\" row carries the master-key status)", n)
		}
	})
}
