package cli

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func setLastResort(t *testing.T, name, value string) (int, string) {
	t.Helper()
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return configureProviderOptions(&configureOpts{}, []string{name, "options.last_resort=" + value})
	})
	return code, stderr
}

func lastResortOf(t *testing.T, name string) bool {
	t.Helper()
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg.Providers[name].Options.LastResort
}

func TestConfigureLastResortOption(t *testing.T) {
	vaulttest.UseFreshKey(t)
	for _, tc := range []struct {
		value string
		want  bool
	}{{"false", false}, {"true", true}} {
		if code, stderr := setLastResort(t, "local", tc.value); code != 0 || lastResortOf(t, "local") != tc.want {
			t.Fatalf("set %s: exit %d stderr %q, got %v", tc.value, code, stderr, lastResortOf(t, "local"))
		}
	}
	if code, stderr := setLastResort(t, "local", "maybe"); code == 0 || !strings.Contains(stderr, "true or false") {
		t.Fatalf("maybe: exit %d stderr %q", code, stderr)
	}
	err := providers.SetProviderFields("bare", map[string]any{
		"protocol": "openai", "base_url": "http://127.0.0.1:1/v1", "auth": map[string]any{"type": "none"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if code, stderr := setLastResort(t, "bare", "true"); code == 0 || !strings.Contains(stderr, "routes.anthropic") ||
		lastResortOf(t, "bare") {
		t.Fatalf("bare true: exit %d stderr %q, stored %v", code, stderr, lastResortOf(t, "bare"))
	}
}
