package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func writeProxyFixture(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func removeProxyFixture(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func TestClientSecretRetriesLegacyDeletionOnVaultHit(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, secretName, "already-imported-secret")
	child := filepath.Join(secretFile(), "blocks-removal")
	writeProxyFixture(t, child, "interrupted migration")
	if _, err := ClientSecret(); err == nil || !strings.Contains(err.Error(), "remove legacy") {
		t.Fatalf("ClientSecret with blocked cleanup = %v, want legacy removal error", err)
	}
	removeProxyFixture(t, child)
	removeProxyFixture(t, secretFile())
	writeProxyFixture(t, secretFile(), "already-imported-secret\n")
	got, err := ClientSecret()
	if err != nil || got != "already-imported-secret" {
		t.Fatalf("ClientSecret retry = %q, %v; want the existing vault secret", got, err)
	}
	if _, err := os.Stat(secretFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy secret survived successful retry: %v", err)
	}
}

func TestClaimRestartRejectsUnrecordableClaim(t *testing.T) {
	for _, failure := range []string{"lock-and-stamp", "stamp-only"} {
		t.Run(failure, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			path := filepath.Join(paths.StateDir(), "proxy.restart-claim")
			if failure == "lock-and-stamp" {
				writeProxyFixture(t, filepath.Dir(paths.StateDir()), "not a directory")
			} else {
				writeProxyFixture(t, filepath.Join(path, "blocks-stamp"), "occupied")
				old := time.Now().Add(-2 * restartRetry)
				if err := os.Chtimes(path, old, old); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if claimRestart(time.Now()) {
					t.Fatal("restart claimed without a writable throttle stamp")
				}
			}
		})
	}
}

func TestRevokeSessionTokenPropagatesStateReadFailure(t *testing.T) {
	for _, kind := range []string{"malformed", "unreadable", "missing"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			switch kind {
			case "malformed":
				writeProxyFixture(t, stateFile(), "{")
			case "unreadable":
				writeProxyFixture(t, filepath.Join(stateFile(), "not-a-state-file"), "occupied")
			}
			_, readErr := ReadState()
			err := RevokeSessionToken(context.Background(), "session-to-revoke")
			if kind == "missing" {
				if err != nil {
					t.Fatalf("missing state: revoke = %v, want nil", err)
				}
				return
			}
			if err == nil || errors.Is(err, ErrNotRunning) {
				t.Fatalf("unreadable state silently treated as stopped: %v", err)
			}
			if !strings.Contains(err.Error(), "proxy: read state: "+readErr.Error()) {
				t.Fatalf("revoke lost state read error: %v; want %v", err, readErr)
			}
		})
	}
}

func proxyOutputFixture(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	previous := holderComm
	t.Cleanup(func() { holderComm = previous })
	holderComm = func(int) string { return "claude-code" }
	probes := 0
	port := occupy(t, func(w http.ResponseWriter, _ *http.Request) {
		probes++
		w.Header().Set(PIDHeader, strconv.Itoa(os.Getpid()))
		if probes == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	cleanup, err := WriteState(State{PID: os.Getpid(), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
}

func TestProxyCommandsRejectFailedStdout(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires /dev/full")
	}
	for _, command := range []struct {
		name string
		run  func(context.Context) int
	}{{"stop", StopWithHold}, {"restart", Restart}} {
		t.Run(command.name, func(t *testing.T) {
			proxyOutputFixture(t)
			full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			previous := os.Stdout
			os.Stdout = full
			t.Cleanup(func() {
				os.Stdout = previous
				if err := full.Close(); err != nil {
					t.Error(err)
				}
			})
			if code := command.run(context.Background()); code == 0 {
				t.Fatal("command returned success after stdout write failed")
			}
		})
	}
}

func TestWriteStateSerializesOwnershipCheckAndPublication(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	done := make(chan error, 1)
	owner := State{PID: os.Getpid(), Port: 19001}
	err := paths.WithFileLock(stateFile(), func() error {
		go func() {
			_, err := WriteState(State{PID: 1 << 30, Port: 19002})
			done <- err
		}()
		select {
		case err := <-done:
			return fmt.Errorf("WriteState bypassed the shared publication lock: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		data := fmt.Sprintf(`{"schema":1,"pid":%d,"port":%d}`, owner.PID, owner.Port)
		return paths.AtomicWrite(stateFile(), []byte(data), 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := ReadState()
	if err != nil || got.PID != owner.PID || got.Port != owner.Port {
		t.Fatalf("competing proxy clobbered owner: %+v, %v; want %+v", got, err, owner)
	}
}
