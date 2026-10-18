package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeTailscaleOnPATH puts a `tailscale` script on PATH answering `status --json` per state.
func fakeTailscaleOnPATH(t *testing.T, backendState string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = status ]; then /bin/cat <<EOF\n" +
		`{"BackendState":"` + backendState + `","Self":{"DNSName":"test-mac.tailxxxxx.ts.net.","AllowedIPs":[]},"Peer":{}}` +
		"\nEOF\nfi\n"
	if err := os.WriteFile(filepath.Join(dir, "tailscale"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func hermeticTailscaleSecrets(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, n := range []string{"TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET", "TAILSCALE_API_KEY"} {
		t.Setenv(n, "")
	}
}

func TestTailscaleStatusNoCLI(t *testing.T) {
	hermeticTailscaleSecrets(t)
	t.Setenv("PATH", t.TempDir())
	code, _, stderr := captureOutput(t, func() int { return cmdTailscale([]string{"status"}) })
	if code == 0 {
		t.Errorf("status with no CLI on PATH: code = 0, want nonzero; stderr=%q", stderr)
	}
}

func TestTailscaleStatusRunningTextAndJSON(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Running")
	code, out, _ := captureOutput(t, func() int { return cmdTailscale([]string{"status"}) })
	if code != 0 {
		t.Fatalf("status exit %d", code)
	}
	if !strings.Contains(out, "test-mac.tailxxxxx.ts.net.") {
		t.Errorf("status text output missing node name:\n%s", out)
	}

	code, out, _ = captureOutput(t, func() int { return cmdTailscale([]string{"status", "--format", "json"}) })
	if code != 0 || !strings.Contains(out, "test-mac.tailxxxxx.ts.net.") {
		t.Errorf("status --format json: code=%d out=%q", code, out)
	}
}

func TestTailscaleConnectWithoutKeyAlreadyRunningExitsZero(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Running")
	code, out, _ := captureOutput(t, func() int { return cmdTailscale([]string{"connect"}) })
	if code != 0 {
		t.Errorf("connect (already running, no key) exit %d, want 0", code)
	}
	if !strings.Contains(out, "already connected") {
		t.Errorf("connect output = %q, want it to say already connected", out)
	}
}

func TestTailscaleConnectWithoutKeyNotRunningExitsTwo(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Stopped")
	code, _, stderr := captureOutput(t, func() int { return cmdTailscale([]string{"connect"}) })
	if code != 2 {
		t.Errorf("connect (not running, no key) exit %d, want 2; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "configure") && !strings.Contains(stderr, "auth-key") {
		t.Errorf("stderr = %q, want a configure/--auth-key hint", stderr)
	}
}

func TestTailscaleKeyWithoutCredentialFails(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Running")
	code, out, stderr := captureOutput(t, func() int { return cmdTailscale([]string{"key"}) })
	if code == 0 {
		t.Fatalf("key with no admin credential: exit 0, want nonzero; stdout=%q", out)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("key must print nothing to stdout on failure, got %q", out)
	}
	if !strings.Contains(stderr, "credential") {
		t.Errorf("stderr = %q, want it to mention the missing credential", stderr)
	}
}

func TestTailscaleExitNodeApproveWithoutCredentialPrintsAdminURL(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Running")
	code, out, _ := captureOutput(t, func() int { return cmdTailscale([]string{"exit-node", "--approve"}) })
	if code != 2 {
		t.Errorf("exit-node --approve (no credential) exit %d, want 2", code)
	}
	if !strings.Contains(out, "login.tailscale.com/admin/machines") {
		t.Errorf("output = %q, want the admin console URL", out)
	}
}

func TestTailscaleUnknownSubcommand(t *testing.T) {
	code, _, stderr := captureOutput(t, func() int { return cmdTailscale([]string{"bogus"}) })
	if code == 0 {
		t.Error("unknown subcommand must not exit 0")
	}
	if !strings.Contains(stderr, "bogus") {
		t.Errorf("stderr = %q, want it to name the bad subcommand", stderr)
	}
}
