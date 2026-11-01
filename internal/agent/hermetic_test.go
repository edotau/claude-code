package agent

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func vendorPATHFixture(t *testing.T, name, body string) string {
	t.Helper()
	cfg := os.Getenv("CLAUDE_CONFIG_DIR")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	bin := pathstest.FakeVendor(t, name, body)
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if name == "agy" {
		dst := filepath.Join(filepath.Dir(bin), "agy")
		if err := os.Rename(bin, dst); err != nil {
			t.Fatal(err)
		}
		bin = dst
	}
	return bin
}

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
	StubRouterForTest(t, "http://127.0.0.1:4000")
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

type newlineRejectWriter struct {
	writes []string
	err    error
}

func (w *newlineRejectWriter) Write(p []byte) (int, error) {
	w.writes = append(w.writes, string(p))
	if string(p) == "\n" {
		return 0, w.err
	}
	return len(p), nil
}
