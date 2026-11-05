package agents

import (
	"bytes"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/worktree"
)

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) {
	agent.NoticeOut = io.Discard
	agent.StubProxyForTest("http://127.0.0.1:9999", "rtr-secret-test") // the batch/envelope tests reach the proxy
	agentconfig.SetCatalogWarn(io.Discard)
	cleanup := hosttest.Setup()
	worktree.StubProcessScanForTest()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// captureOutput redirects os.Stdout/os.Stderr around fn and returns fn's exit code plus both streams.
func captureOutput(t *testing.T, fn func() int) (code int, stdout, stderr string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	ro, wo, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	re, we, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var bo, be bytes.Buffer
	var drained sync.WaitGroup
	drained.Go(func() { io.Copy(&bo, ro) }) // drain while fn runs: a full pipe would block its writes forever
	drained.Go(func() { io.Copy(&be, re) })
	os.Stdout, os.Stderr = wo, we
	code = fn()
	wo.Close()
	we.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	drained.Wait()
	return code, bo.String(), be.String()
}

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	_, stdout, _ := captureOutput(t, func() int { fn(); return 0 })
	return stdout
}
