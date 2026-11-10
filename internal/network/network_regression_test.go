package network

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/textutil"
)

func fakeAdvertiseCLI(t *testing.T, exitCode int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	dir := t.TempDir()
	argv := filepath.Join(dir, "argv")
	script := "#!/bin/sh\nif [ \"$1\" = status ]; then\n" +
		"printf '%s\\n' '{\"BackendState\":\"Running\",\"Self\":{\"DNSName\":\"test-node.ts.net.\"}}'\nexit 0\nfi\n" +
		"printf '%s\\n' \"$@\" > " + textutil.ShellQuote(argv) + "\n" + fmt.Sprintf("exit %d\n", exitCode)
	if err := os.WriteFile(filepath.Join(dir, "tailscale"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return argv
}

func TestTailscaleKeylessConnectAppliesAdvertise(t *testing.T) {
	for _, exitCode := range []int{0, 1} {
		t.Run(fmt.Sprintf("set_exit_%d", exitCode), func(t *testing.T) {
			hermeticTailscaleSecrets(t)
			argv := fakeAdvertiseCLI(t, exitCode)
			code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
				return cmdNetworkTailscale([]string{"--connect", "--advertise-exit"})
			})
			if code != exitCode {
				t.Fatalf("advertise exit=%d, want %d; stdout=%q stderr=%q", code, exitCode, stdout, stderr)
			}
			got, err := os.ReadFile(argv)
			if err != nil {
				t.Fatalf("keyless connect never applied advertising: %v", err)
			}
			if string(got) != "set\n--advertise-exit-node\n" {
				t.Fatalf("advertise argv=%q", got)
			}
		})
	}
}

func captureClosedStdout(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		if err := os.Stdout.Close(); err != nil {
			t.Fatal(err)
		}
		return fn()
	})
	return code, stderr
}

func TestTailscaleKeyReportsStdoutFailure(t *testing.T) {
	hermeticTailscaleSecrets(t)
	t.Setenv("TAILSCALE_API_KEY", "test-api-key")
	srv := tailscaleAuthKeyServer(t, "tskey-auth-test-output")
	defer srv.Close()
	originalClient := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = originalClient })
	http.DefaultClient = &http.Client{Transport: tailscaleTestTransport(func(r *http.Request) (*http.Response, error) {
		req := r.Clone(r.Context())
		req.URL.Scheme, req.URL.Host = "http", strings.TrimPrefix(srv.URL, "http://")
		return srv.Client().Do(req)
	})}
	code, stderr := captureClosedStdout(t, func() int { return cmdNetworkTailscale([]string{"--key"}) })
	if code != 1 || !strings.Contains(stderr, "writing key to stdout") {
		t.Fatalf("undelivered auth key: exit=%d stderr=%q", code, stderr)
	}
	if strings.Contains(stderr, "tskey-auth-test-output") {
		t.Fatalf("auth key leaked into stderr: %q", stderr)
	}
}

func TestRemoteTextReportsStdoutFailure(t *testing.T) {
	for _, status := range []bool{false, true} {
		t.Run(fmt.Sprintf("status_%t", status), func(t *testing.T) {
			hermeticDispatch(t)
			args := []string{"--list", "--host", "myhost"}
			want := "network remote --list:"
			if status {
				fakeSSHStatusOnPATH(t, statusCaptureFrame("cc-claude-ab12", "%1", "claude", "claude", "thinking..."))
				args = append(args, "--status")
				want = "network remote --status:"
			} else {
				fakeSSHOnPATH(t, "cc-claude-ab12|1700000000|1|claude|claude\n")
			}
			code, stderr := captureClosedStdout(t, func() int { return cmdNetworkRemote(args) })
			if code != 1 || !strings.Contains(stderr, want) || !strings.Contains(stderr, "file already closed") {
				t.Fatalf("failed text output reported success: exit=%d stderr=%q", code, stderr)
			}
		})
	}
}
