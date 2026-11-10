package menu

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/network/tailscale"
)

// resetTailscaleStatus empties the home page's Status cache for this test and restores its seams after.
func resetTailscaleStatus(t *testing.T) {
	t.Helper()
	origTTL, origFetch := tsStatusTTL, tsStatusFetch
	tsStatus.Lock()
	tsStatus.at = time.Time{}
	tsStatus.Unlock()
	t.Cleanup(func() {
		tsStatus.Lock()
		tsStatus.at = time.Time{}
		tsStatus.Unlock()
		tsStatusTTL, tsStatusFetch = origTTL, origFetch
	})
}

// TestTailscaleStatusCachedWithinTTL: a render and runRemote inside the TTL share one Status call; expiry refetches.
func TestTailscaleStatusCachedWithinTTL(t *testing.T) {
	resetTailscaleStatus(t)
	calls := 0
	tsStatusFetch = func(context.Context) (tailscale.Local, error) {
		calls++
		return tailscale.Local{DNSName: "box.tailxxxxx.ts.net."}, errors.New("stopped")
	}
	for range 3 {
		if _, err := tailscaleStatus(context.Background()); err == nil || err.Error() != "stopped" {
			t.Fatalf("cached err = %v, want the fetched error", err)
		}
	}
	if calls != 1 {
		t.Fatalf("Status calls within TTL = %d, want 1", calls)
	}
	tsStatusTTL = 0
	if st, _ := tailscaleStatus(context.Background()); calls != 2 || st.DNSName != "box.tailxxxxx.ts.net." {
		t.Errorf("after TTL: calls = %d, DNSName %q; want a refetch", calls, st.DNSName)
	}
}

// TestWebSearchClaudeWiredHonoursConfigDir: the claude client reads CLAUDE_CONFIG_DIR/.claude.json, not $HOME/.claude.json.
func TestWebSearchClaudeWiredHonoursConfigDir(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	doc := `{"mcpServers":{"web_search":{"command":"/x/claude-code","args":["network","websearch"]}}}`
	if err := os.WriteFile(filepath.Join(cfg, ".claude.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if !webSearchClients()[0].wired() {
		t.Fatal("claude client must read the wired .claude.json under CLAUDE_CONFIG_DIR")
	}
}

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
func runRemoteOut(t *testing.T) string {
	t.Helper()
	t.Setenv("TAILSCALE_OAUTH_CLIENT_ID", "")
	t.Setenv("TAILSCALE_OAUTH_CLIENT_SECRET", "")
	t.Setenv("TAILSCALE_API_KEY", "")
	resetTailscaleStatus(t)
	var out bytes.Buffer
	d := &dialog{o: Options{}, out: &out}
	runRemote(d)
	return out.String()
}

// Tailscale connected: the remote section reports it running, by DNS name, and never fails.
func TestRunRemoteConnectedReportsStatus(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	fakeTailscaleOnPATH(t, "test-mac.tailxxxxx.ts.net.")

	got := runRemoteOut(t)
	if !strings.Contains(got, "tailscale   running (test-mac.tailxxxxx.ts.net)") {
		t.Errorf("output missing the running line:\n%s", got)
	}
	if !strings.Contains(got, "admin api   credential absent") {
		t.Errorf("output should note the missing admin credential:\n%s", got)
	}
}

// Tailscale not on PATH: the remote section still reports (not connected), and --check never fails
// on it — this is the contract the old tailscale key row enforced, now owned by `remote`.
func TestRunRemoteNotConnectedNeverFails(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no `tailscale` CLI at all
	t.Cleanup(tailscale.StubMacAppCLI(""))

	got := runRemoteOut(t)
	if !strings.Contains(got, "tailscale   not connected → claude-code network tailscale --connect") {
		t.Errorf("output missing the not-connected line:\n%s", got)
	}
	if !strings.Contains(got, "dispatch    could not list tailnet machines:") {
		t.Errorf("output missing the machine-list error:\n%s", got)
	}
	if strings.Contains(got, "no online tailnet machines found") {
		t.Errorf("machine-list failure was misreported as an empty list:\n%s", got)
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
	runRemote(d)
	got := out.String()
	if !strings.Contains(got, "admin api   credential present") {
		t.Errorf("output should report the admin credential present:\n%s", got)
	}
	if strings.Contains(got, "tskey-api-abc") {
		t.Errorf("output leaked the credential value: %q", got)
	}
}
