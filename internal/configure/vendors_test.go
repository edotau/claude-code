package configure

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// An agy that ignores SIGINT and SIGTERM is SIGKILLed after the grace, so stopAgy returns instead of hanging.
func TestStopAgyEscalatesToKill(t *testing.T) {
	orig := AgyStopGrace
	AgyStopGrace = 100 * time.Millisecond
	t.Cleanup(func() { AgyStopGrace = orig })
	cmd := exec.Command("sh", "-c", `trap "" INT TERM; echo ready; while :; do sleep 0.05; done`)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := out.Read(make([]byte, 6)); err != nil { // "ready\n": the traps are installed
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	stopped := make(chan struct{})
	go func() { stopAgy(cmd, done); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("stopAgy did not return for an agy that ignores SIGINT and SIGTERM")
	}
}

func TestStopAgyKillsStubbornDescendant(t *testing.T) {
	if os.Getenv("CONFIGURE_DESCENDANT_HELPER") == "1" {
		signal.Ignore(syscall.SIGINT, syscall.SIGTERM)
		fmt.Fprintln(os.Stdout, os.Getpid())
		for {
			time.Sleep(time.Hour)
		}
	}
	orig := AgyStopGrace
	AgyStopGrace = 100 * time.Millisecond
	t.Cleanup(func() { AgyStopGrace = orig })
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	cmd := exec.Command("sh", "-c", `trap 'exit 0' INT TERM; "$CONFIGURE_TEST_BINARY" -test.run=^TestStopAgyKillsStubbornDescendant$ & wait`)
	cmd.Env = append(os.Environ(), "CONFIGURE_DESCENDANT_HELPER=1", "CONFIGURE_TEST_BINARY="+bin)
	cmd.Stdout = w
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	_ = w.Close()
	var pid int
	if _, err := fmt.Fscanln(r, &pid); err != nil {
		t.Fatal(err)
	}
	child, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Kill() })
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	exited := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, r); close(exited) }()
	stopAgy(cmd, done)
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("descendant still holds stdout open after its parent exited and stopAgy returned")
	}
}

func TestHasAdminCredentialRequiresCompleteCredential(t *testing.T) {
	for _, tc := range []struct {
		name, id, secret, key string
		present               bool
	}{
		{"none", "", "", "", false},
		{"id only", "client-id", "", "", false},
		{"secret only", "", "client-secret", "", false},
		{"oauth pair", "client-id", "client-secret", "", true},
		{"api key", "", "", "api-key", true},
		{"api key with incomplete oauth", "client-id", "", "api-key", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vaulttest.Use(t)
			for name, value := range map[string]string{
				"TAILSCALE_OAUTH_CLIENT_ID":     tc.id,
				"TAILSCALE_OAUTH_CLIENT_SECRET": tc.secret,
				"TAILSCALE_API_KEY":             tc.key,
			} {
				t.Setenv(name, "")
				if value != "" {
					vaulttest.Secret(t, name, value)
				}
			}
			if got := HasAdminCredential(); got != tc.present {
				t.Fatalf("HasAdminCredential = %v, want %v", got, tc.present)
			}
		})
	}
}

// Without a resolvable home the login marker would be a cwd-relative path: refuse before launching agy.
