package statusline

import (
	"github.com/edotau/claude-code/internal/ui/theme"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The default order puts the context bar right after the model, then the rate limits; the agent count is on.
func TestDefaultConfigOrder(t *testing.T) {
	hermetic(t)
	cfg := loadConfig()
	want := []string{"provider", "model", "proxy", "context", "limits", "total", "duration", "session"}
	if !reflect.DeepEqual(cfg.Order, want) {
		t.Errorf("Order = %v, want %v", cfg.Order, want)
	}
	if !cfg.seg("limits", false) {
		t.Error("segments.limits should default to true")
	}
}

// The provider segment is on by default; the statusline shows which provider the session is on.
func TestDefaultConfigProviderOn(t *testing.T) {
	hermetic(t)
	if !loadConfig().seg("provider", false) {
		t.Error("segments.provider should default to true")
	}
}

func writeStatuslineConfig(t *testing.T, data string) {
	t.Helper()
	dir := theme.StatuslineDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidConfigIgnoredWhole(t *testing.T) {
	hermetic(t)
	writeStatuslineConfig(t, `{"style":"minimal","width":"invalid"}`)
	if got, want := loadConfig(), embeddedConfig(); !reflect.DeepEqual(got, want) {
		t.Fatalf("invalid config partially applied: got %+v, want %+v", got, want)
	}
}

func TestRejectedConfigPreservesFallbackSlices(t *testing.T) {
	hermetic(t)
	want := defaultConfig()
	writeStatuslineConfig(t, `{"order":["rejected"],"dashboard":{"columns":["rejected"]},"width":"invalid"}`)
	if got := loadConfig(); !reflect.DeepEqual(got, want) {
		t.Fatalf("rejected config changed fallback: got %+v, want %+v", got, want)
	}
	if err := os.Remove(filepath.Join(theme.StatuslineDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	if got := loadConfig(); !reflect.DeepEqual(got, want) {
		t.Fatalf("rejected config corrupted cached defaults: got %+v, want %+v", got, want)
	}
}
