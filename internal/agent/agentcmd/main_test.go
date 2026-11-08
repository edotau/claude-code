package agentcmd

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	_ "github.com/edotau/claude-code/internal/claude"
	_ "github.com/edotau/claude-code/internal/codex"
	_ "github.com/edotau/claude-code/internal/copilot"
	_ "github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/hosttest"
	_ "github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestMain runs in the hermetic host (hosttest) with the vendor roster registered and the proxy stubbed.
func TestMain(m *testing.M) {
	agent.NoticeOut = io.Discard
	agentconfig.SetCatalogWarn(io.Discard)
	agent.StubProxyForTest("http://127.0.0.1:9999", "test-router-secret")
	os.Exit(hosttest.Main(m))
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

// useVaultTest points the vault at a temp dir with a throwaway master key.
func useVaultTest(t *testing.T) string {
	t.Helper()
	dir := vaulttest.Use(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(key))
	return dir
}

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	_, stdout, _ := captureOutput(t, func() int { fn(); return 0 })
	return stdout
}
