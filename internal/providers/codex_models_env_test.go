package providers

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestCodexCachedModelsHonoursCodexHome: models_cache.json is read from $CODEX_HOME, not always ~/.codex.
func TestCodexCachedModelsHonoursCodexHome(t *testing.T) {
	codex := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", codex)
	cache := `{"models":[{"slug":"gpt-x","display_name":"X"}]}`
	if err := os.WriteFile(filepath.Join(codex, "models_cache.json"), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := CodexCachedModels()
	if err != nil || len(got) != 1 || got[0].ID != "gpt-x" {
		t.Fatalf("CodexCachedModels = %+v, %v; want [gpt-x]", got, err)
	}
}

// TestHTTPClientHasTimeout: a hung endpoint must not block forever, yet DefaultClient's Transport seam is kept.
func TestHTTPClientHasTimeout(t *testing.T) {
	c := httpClient()
	if c.Timeout <= 0 {
		t.Fatalf("httpClient().Timeout = %v; want > 0", c.Timeout)
	}
	if c.Transport != http.DefaultClient.Transport {
		t.Fatal("httpClient must keep http.DefaultClient's Transport")
	}
}
