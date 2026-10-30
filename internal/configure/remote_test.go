package configure

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/network/tailscale"
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
		`{"BackendState":"Running","Self":{"DNSName":"` + dnsName + `","TailscaleIPs":["100.64.1.2"],"AllowedIPs":[]},"Peer":{}}` +
		"\nEOF\nfi\n"
	if err := os.WriteFile(filepath.Join(dir, "tailscale"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// runRemoteOut runs the remote section into a buffer with every tailscale admin credential cleared.
func runRemoteOut(t *testing.T) (string, bool) {
	t.Helper()
	t.Setenv("TAILSCALE_OAUTH_CLIENT_ID", "")
	t.Setenv("TAILSCALE_OAUTH_CLIENT_SECRET", "")
	t.Setenv("TAILSCALE_API_KEY", "")
	var out bytes.Buffer
	d := &dialog{o: Options{}, out: &out}
	ok := runRemote(d)
	return out.String(), ok
}

// Tailscale connected: the remote section reports it running, by DNS name, and never fails.
func TestRunRemoteConnectedReportsStatus(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	fakeTailscaleOnPATH(t, "test-mac.tailxxxxx.ts.net.")

	got, ok := runRemoteOut(t)
	if !ok {
		t.Fatal("runRemote = false, want true (read-only; never fails)")
	}
	if !strings.Contains(got, "tailscale   running (test-mac.tailxxxxx.ts.net)") {
		t.Errorf("output missing the running line:\n%s", got)
	}
	if !strings.Contains(got, "admin api   credential absent") {
		t.Errorf("output should note the missing admin credential:\n%s", got)
	}
	if !strings.Contains(got, "web         100.64.1.2 available for claude-code network page") {
		t.Errorf("output missing the tailnet web-bind line:\n%s", got)
	}
}

// Tailscale not on PATH: the remote section still reports (not connected), and --check never fails
// on it — this is the contract the old tailscale key row enforced, now owned by `remote`.
func TestRunRemoteNotConnectedNeverFails(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no `tailscale` CLI at all

	got, ok := runRemoteOut(t)
	if !ok {
		t.Fatal("runRemote = false, want true even when tailscale is absent")
	}
	if !strings.Contains(got, "tailscale   not connected → claude-code network tailscale --connect") {
		t.Errorf("output missing the not-connected line:\n%s", got)
	}
	if !strings.Contains(got, "web         no tailnet IPv4 address; claude-code network page binds loopback only") {
		t.Errorf("output missing the no-IPv4 web line:\n%s", got)
	}
	if !strings.Contains(got, "dispatch    no online tailnet machines found") {
		t.Errorf("output missing the no-machines dispatch line:\n%s", got)
	}
}

// hasAdminCredential notes a configured admin credential by name, but never leaks the value, and the
// remote section's running case reflects it.
func TestRunRemoteNotesConfiguredAdminCredential(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	fakeTailscaleOnPATH(t, "test-mac.tailxxxxx.ts.net.")
	t.Setenv("TAILSCALE_OAUTH_CLIENT_ID", "")
	t.Setenv("TAILSCALE_OAUTH_CLIENT_SECRET", "")
	t.Setenv("TAILSCALE_API_KEY", "tskey-api-abc")

	var out bytes.Buffer
	d := &dialog{o: Options{}, out: &out}
	if !runRemote(d) {
		t.Fatal("runRemote = false, want true")
	}
	got := out.String()
	if !strings.Contains(got, "admin api   credential present") {
		t.Errorf("output should report the admin credential present:\n%s", got)
	}
	if strings.Contains(got, "tskey-api-abc") {
		t.Errorf("output leaked the credential value: %q", got)
	}
}

func TestHasAdminCredential(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TAILSCALE_OAUTH_CLIENT_ID", "")
	t.Setenv("TAILSCALE_OAUTH_CLIENT_SECRET", "")
	t.Setenv("TAILSCALE_API_KEY", "")
	t.Setenv("PATH", t.TempDir()) // no secret-tool: the host keyring must not answer
	if hasAdminCredential() {
		t.Error("hasAdminCredential() = true with nothing set, want false")
	}
	t.Setenv("TAILSCALE_API_KEY", "tskey-api-abc")
	if !hasAdminCredential() {
		t.Error("hasAdminCredential() = false with TAILSCALE_API_KEY set, want true")
	}
}

func TestTailnetIPv4PicksCGNATAddress(t *testing.T) {
	st := tailscale.Local{IPs: []string{"fd7a:115c:a1e0::1", "100.64.1.2", "192.168.1.5"}}
	if got := tailnetIPv4(st); got != "100.64.1.2" {
		t.Errorf("tailnetIPv4 = %q, want 100.64.1.2", got)
	}
	if got := tailnetIPv4(tailscale.Local{IPs: []string{"192.168.1.5"}}); got != "" {
		t.Errorf("tailnetIPv4 = %q, want \"\" (no CGNAT address)", got)
	}
}

// Dropping tailscale from `configure keys`/secrets is deliberate (BREAKING CHANGE): no row, no
// TAILSCALE_OAUTH_CLIENT_SECRET console link — both now live only in the read-only remote section.
func TestKeysAndSecretsNeverCarryTailscale(t *testing.T) {
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
	if hasLabel(all, "tailscale") {
		t.Error(`keyTargets("") must no longer carry a tailscale row (moved to the remote section)`)
	}
	if consoleURLs["TAILSCALE_OAUTH_CLIENT_SECRET"] != "" {
		t.Error("consoleURLs must no longer carry TAILSCALE_OAUTH_CLIENT_SECRET (configure keys no longer prompts for it)")
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
