//go:build unix

package proxy

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func restartChild(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
		var exitErr *exec.ExitError
		if err := <-done; err != nil && !errors.As(err, &exitErr) {
			t.Error(err)
		}
	})
	return cmd.Process.Pid
}

func restartOutputFailure(t *testing.T) string {
	t.Helper()
	output, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	diagnostic, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	previousOut, previousErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = output, diagnostic
	t.Cleanup(func() {
		os.Stdout, os.Stderr = previousOut, previousErr
		if err := diagnostic.Close(); err != nil {
			t.Error(err)
		}
	})
	return diagnostic.Name()
}

func TestRestartAttemptsEnsureAfterStoppedOutputFailure(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, secretName, "restart-test-secret")
	pid := restartChild(t)
	previous := holderComm
	holderComm = func(int) string { return "claude-code" }
	t.Cleanup(func() { holderComm = previous })
	port := occupy(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(PIDHeader, strconv.Itoa(pid))
	})
	cleanup, err := WriteState(State{PID: pid, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	diagnostic := restartOutputFailure(t)
	if code := Restart(context.Background()); code == 0 {
		t.Fatal("Restart returned success after a failed stdout write")
	}
	data, err := os.ReadFile(diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "proxy: write output:") {
		t.Fatalf("Stop did not reach its failed progress write: %s", data)
	}
	if !strings.Contains(string(data), ErrTestBinary.Error()) {
		t.Fatalf("Restart did not attempt Ensure after the progress write failed: %s", data)
	}
}
