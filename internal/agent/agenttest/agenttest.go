// Package agenttest holds the test fixtures the vendor packages share; package agent itself cannot import it.
package agenttest

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Scratch sets a hermetic config dir with a keyed default provider (every vendor renders); returns the vendor home.
func Scratch(t *testing.T) string {
	t.Helper()
	configDir := vaulttest.Use(t)
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("GEMINI_CLI_HOME", filepath.Join(home, ".gemini-cli-home-unused"))
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex-home-unused"))
	// Isolated from the real machine's opencode auth.json/opencode.jsonc (ResolveTarget probes both unconditionally).
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("PATH", t.TempDir()) // no real opencode: a 2.x CLI would report its own logins instead of auth.json
	t.Setenv("HARNESS_PROVIDER", "")
	t.Setenv("GEMINI_API_KEY", "sk-test-gemini")
	t.Setenv("OPENROUTER_API_KEY", "sk-test-openrouter")
	t.Cleanup(agent.StubRouter("http://127.0.0.1:9999"))
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	vaulttest.Put(t, vault.Overlay, "providers", `{"default": "openai"}`)
	if err := os.MkdirAll(filepath.Join(configDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	hosttest.WriteRel(t, configDir, "CLAUDE.md", "# CLAUDE.md\n\nfixture\n")
	return home
}

// AssertNotExist reports each path that exists though it must not.
func AssertNotExist(t testing.TB, why string, ps ...string) {
	t.Helper()
	for _, p := range ps {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s: %s, stat err = %v", p, why, err)
		}
	}
}

// AssertExists reports each path that is missing though it must exist.
func AssertExists(t testing.TB, why string, ps ...string) {
	t.Helper()
	for _, p := range ps {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: %s: %v", p, why, err)
		}
	}
}

// LaunchHermetic isolates a launch test from the host and stubs the router, with launch notices discarded.
func LaunchHermetic(t testing.TB) {
	t.Helper()
	hosttest.HermeticEnv(t)
	t.Cleanup(agent.StubRouter("http://127.0.0.1:4000"))
	orig := agent.NoticeOut
	agent.NoticeOut = io.Discard
	t.Cleanup(func() { agent.NoticeOut = orig })
}

// LoadTarget is provider's built-in row with every provider key set, under a fresh config dir.
func LoadTarget(t testing.TB, provider, model string) providers.Target {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{"OPENAI_API_KEY", "GEMINI_API_KEY", "GATEWAY_API_KEY", "ANTHROPIC_API_KEY"} {
		t.Setenv(k, "sk-SECRET-123")
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, err := reg.Get(provider)
	if err != nil {
		t.Fatal(err)
	}
	return providers.Target{Provider: p, Model: model}
}

// StubProxy stubs the router at base, its secret and session mints for the test.
func StubProxy(t testing.TB, base, secret string) {
	t.Helper()
	oe, os_, om := agent.EnsureRouter, agent.RouterSecret, agent.MintSession
	t.Cleanup(func() { agent.EnsureRouter, agent.RouterSecret, agent.MintSession = oe, os_, om })
	agent.StubProxyForTest(base, secret)
}

// SocketAccepts dials rather than stats: the socket file exists a moment before the holder listens on it.
func SocketAccepts(path string) bool {
	c, err := (&net.Dialer{Timeout: time.Second}).DialContext(context.Background(), "unix", path)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// StartHolder claims a live record for name (l may be nil) and runs ServeLive; the returned func waits for its error.
func StartHolder(t testing.TB, ctx context.Context, name string, idle time.Duration, l agent.Launcher) (string, func() error) {
	t.Helper()
	rec := agent.ClaimRecord(name, agent.RunOptions{Timeout: idle}, time.Now())
	rec.Live, rec.PID = true, os.Getpid()
	rec.Request.Launcher = l
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	done, id := make(chan error, 1), rec.ID
	go func() { done <- agent.ServeLive(ctx, &rec) }()
	for range 200 {
		if SocketAccepts(agent.LiveSocket(id)) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if _, err := os.Stat(agent.LiveSocket(id)); err != nil {
		select {
		case serveErr := <-done:
			t.Fatalf("holder did not open its socket: %v (serve: %v)", err, serveErr)
		default:
			t.Fatalf("holder did not open its socket: %v", err)
		}
	}
	return id, func() error { return <-done }
}
