package configure

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// fakeTailscaleOnPATH puts a `tailscale` script on PATH that answers `status --json` with a
// Running fixture (self only, no peers) so tailscale.Status resolves without the real daemon.
func fakeTailscaleOnPATH(t *testing.T, dnsName string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = status ]; then /bin/cat <<EOF\n" +
		`{"BackendState":"Running","Self":{"DNSName":"` + dnsName + `","AllowedIPs":[]},"Peer":{}}` +
		"\nEOF\nfi\n"
	if err := os.WriteFile(filepath.Join(dir, "tailscale"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestTailscaleKeyTargetPresentReadsDNSName(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	fakeTailscaleOnPATH(t, "test-mac.tailxxxxx.ts.net.")
	t.Setenv("TAILSCALE_OAUTH_CLIENT_ID", "")
	t.Setenv("TAILSCALE_API_KEY", "")

	kt := tailscaleKeyTarget()
	if !kt.present {
		t.Fatalf("present = false, want true (tailscale status --json Running); source=%q", kt.source)
	}
	if !strings.Contains(kt.source, "test-mac.tailxxxxx.ts.net.") {
		t.Errorf("source = %q, missing the DNS name", kt.source)
	}
	if !strings.Contains(kt.source, "no admin credential") {
		t.Errorf("source = %q, want it to note the missing admin credential", kt.source)
	}
}

func TestTailscaleKeyTargetAbsentNamesLoginAndFix(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no `tailscale` on PATH at all
	t.Setenv("TAILSCALE_OAUTH_CLIENT_ID", "")
	t.Setenv("TAILSCALE_API_KEY", "")

	kt := tailscaleKeyTarget()
	if kt.present {
		t.Fatal("present = true, want false (no CLI on PATH)")
	}
	if kt.label != "tailscale" {
		t.Errorf("label = %q, want tailscale", kt.label)
	}
	if len(kt.loginArgv) == 0 || kt.loginArgv[0] != "tailscale" {
		t.Errorf("loginArgv = %v, want it to start with tailscale", kt.loginArgv)
	}
	if kt.envVar != "TAILSCALE_OAUTH_CLIENT_SECRET" {
		t.Errorf("envVar = %q, want TAILSCALE_OAUTH_CLIENT_SECRET", kt.envVar)
	}
	if !strings.Contains(kt.fixCmd, "tailscale login") || !strings.Contains(kt.fixCmd, "TAILSCALE_OAUTH_CLIENT_SECRET") {
		t.Errorf("fixCmd = %q", kt.fixCmd)
	}
}

func TestTailscaleKeyTargetNotesConfiguredAdminCredential(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	fakeTailscaleOnPATH(t, "test-mac.tailxxxxx.ts.net.")
	t.Setenv("TAILSCALE_OAUTH_CLIENT_ID", "")
	t.Setenv("TAILSCALE_API_KEY", "tskey-api-abc")

	kt := tailscaleKeyTarget()
	if !strings.Contains(kt.source, "admin credential TAILSCALE_API_KEY") {
		t.Errorf("source = %q, want it to name TAILSCALE_API_KEY as the admin credential", kt.source)
	}
	if strings.Contains(kt.source, "tskey-api-abc") {
		t.Errorf("source leaked the credential value: %q", kt.source)
	}
}

// TestConsoleURLsHasTailscaleOAuth guards the admin-console link `configure` offers before the paste.
func TestConsoleURLsHasTailscaleOAuth(t *testing.T) {
	if consoleURLs["TAILSCALE_OAUTH_CLIENT_SECRET"] == "" {
		t.Error("consoleURLs lacks TAILSCALE_OAUTH_CLIENT_SECRET")
	}
}

// TestKeyTargetsAppendsTailscaleUnfiltered: the unfiltered keys listing carries a tailscale row;
// a vendor-filtered listing (used by `configure --vendor <v>`) must not.
func TestKeyTargetsAppendsTailscaleUnfiltered(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	all, err := keyTargets(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	if !hasLabel(all, "tailscale") {
		t.Error("keyTargets(\"\") is missing the tailscale row")
	}
	filtered, err := keyTargets(reg, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if hasLabel(filtered, "tailscale") {
		t.Error(`keyTargets(reg, "codex") must not carry the tailscale row`)
	}
}

func hasLabel(ts []keyTarget, label string) bool {
	for _, kt := range ts {
		if kt.label == label {
			return true
		}
	}
	return false
}
