package cli

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/platform/hosttest"
	"github.com/edotau/claude-code/internal/platform/vault/vaulttest"
	"github.com/edotau/claude-code/internal/routing/providers"
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
}

func loopbackRow(url string) map[string]any {
	return map[string]any{"protocol": "openai", "base_url": url, "concurrency": 1,
		"routes": map[string]any{"anthropic": url}, "auth": map[string]any{"type": "none"}}
}

func TestConfigureLastResortGuards(t *testing.T) {
	vaulttest.UseFreshKey(t)
	if err := providers.SetProviderFields("second", loopbackRow("http://127.0.0.1:2/v1")); err != nil {
		t.Fatal(err)
	}
	if code, stderr := setLastResort(t, "second", "true"); code == 0 || !strings.Contains(stderr, "local") ||
		lastResortOf(t, "second") {
		t.Fatalf("second holder: exit %d stderr %q, stored %v", code, stderr, lastResortOf(t, "second"))
	}
}

func TestConfigureLastResortRefusals(t *testing.T) {
	vaulttest.UseFreshKey(t)
	if code, _ := setLastResort(t, "local", "false"); code != 0 {
		t.Fatal("release local failed")
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

func TestConfigureLastResortRemote(t *testing.T) {
	vaulttest.UseFreshKey(t)
	if code, _ := setLastResort(t, "local", "false"); code != 0 {
		t.Fatal("release local failed")
	}
	if err := providers.SetProviderFields("remote", loopbackRow("https://remote.example/v1")); err != nil {
		t.Fatal(err)
	}
	if code, stderr := setLastResort(t, "remote", "true"); code == 0 || !strings.Contains(stderr, "loopback") ||
		lastResortOf(t, "remote") {
		t.Fatalf("remote url: exit %d stderr %q, stored %v", code, stderr, lastResortOf(t, "remote"))
	}
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return configureProviderOptions(&configureOpts{}, []string{"remote"}) })
	if code == 0 || !strings.Contains(stderr, "Databricks preset") {
		t.Fatalf("no pairs on a non-preset row: exit %d stderr %q", code, stderr)
	}
}
