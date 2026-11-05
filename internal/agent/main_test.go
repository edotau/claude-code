package agent

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) {
	NoticeOut = io.Discard
	agentconfig.SetCatalogWarn(io.Discard)
	StubProxyForTest(testRouterBase, testRouterSecret)
	os.Exit(hosttest.Main(m))
}

// quietStderr sends os.Stderr to /dev/null for the test; production notices read os.Stderr at call time.
func quietStderr(t *testing.T) {
	t.Helper()
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = null
	t.Cleanup(func() { os.Stderr = old; null.Close() })
}

// activateAgents turns the external agents on (an external agent with no profile is off) via registry profiles.
func activateAgents(t *testing.T, agents ...string) {
	t.Helper()
	profiles := map[string]string{"codex": "chatgpt", "gemini": "antigravity", "copilot": "personal", "opencode": "nvidia"}
	if err := vault.UpdateAgentRecords(func(m map[string]vault.AgentRecord) {
		for _, a := range agents {
			r := m[a]
			r.Profile = profiles[a]
			m[a] = r
		}
	}); err != nil {
		t.Fatal(err)
	}
}

func allConfigured(string, router.Cell) error { return nil }

var now0 = time.Date(2026, 10, 25, 2, 6, 0, 0, time.Local)

const secret = "sk-SECRET-123"

// fakeJWT is an unsigned token whose iss claim is https://<iss>.
func fakeJWT(iss string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(`{"iss":"https://`+iss+`"}`)) + ".sig"
}

// stubEngine is the Name/Caps/Available base for runner stubs; embed it and add Run.
type stubEngine struct {
	name string // defaults to "fake"
	caps Caps
}

func (s stubEngine) Name() string {
	if s.name != "" {
		return s.name
	}
	return "fake"
}
func (s stubEngine) Caps() Caps       { return s.caps }
func (s stubEngine) Available() error { return nil }

func hermetic(t *testing.T, userProviders string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("BASH_COMPLETION_USER_DIR", "")
	for _, k := range []string{providers.EnvSessionProfile, "CLAUDE_MODEL", "CLAUDE_OPUS_MODEL", "CLAUDE_SONNET_MODEL",
		"CLAUDE_HAIKU_MODEL", "CLAUDE_FABLE_MODEL", "ANTHROPIC_API_KEY", "TEST_GW_KEY"} {
		t.Setenv(k, "")
	}
	if userProviders != "" {
		vaulttest.Put(t, vault.Overlay, "providers", userProviders)
	}
	t.Cleanup(StubRouter("http://127.0.0.1:4000"))
}

func writeFile(path, body string) error { return os.WriteFile(path, []byte(body), 0o600) }

// registry points two test providers (one per dialect) at srv.
func registry(t *testing.T, srv *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("TEST_ANTH_KEY", "anth-key")
	t.Setenv("TEST_OAI_KEY", "oai-key")
	body := fmt.Sprintf(`{"providers":{
	  "anthropic":{"tanth":{"protocol":"anthropic","base_url":%q,"auth":{"type":"key","header":"x-api-key","env":"TEST_ANTH_KEY"},"models":{"opus":"claude-opus-5","haiku":"claude-haiku-4-5"}}},
	  "openai":{"toai":{"protocol":"openai","base_url":%q,"auth":{"type":"key","env":"TEST_OAI_KEY"},"models":{"opus":"gpt-x"}}}}}`, srv.URL, srv.URL+"/v1")
	vaulttest.Put(t, vault.Overlay, "providers", body)
}

func target(t *testing.T, provider, model string) providers.Target {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", t.TempDir()) // isolates GeminiLoginPresent's AgyLoginMarker lookup from the real machine
	writeGatewayOverlay(t)
	for _, k := range []string{"OPENAI_API_KEY", "GEMINI_API_KEY", "GATEWAY_API_KEY", "ANTHROPIC_API_KEY"} {
		t.Setenv(k, secret)
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

// writeGatewayOverlay registers a test-only third-party gateway (no such provider is built in).
func writeGatewayOverlay(t *testing.T) {
	t.Helper()
	body := `{"providers":{"anthropic":{"gateway":{"protocol":"anthropic","base_url":"https://gateway.example.com/api","routes":{"openai":"/v1"},"auth":{"type":"key","header":"bearer","env":"GATEWAY_API_KEY"},"models":{"opus":"anthropic/claude-opus-5","sonnet":"anthropic/claude-sonnet-5","haiku":"anthropic/claude-haiku-4-5"}}}}}`
	vaulttest.Put(t, vault.Overlay, "providers", body)
}
