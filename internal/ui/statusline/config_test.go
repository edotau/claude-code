package statusline

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/edotau/claude-code/internal/ui/theme"
)

// No config.json means the embedded theme; a file overlays its fields and its segments map merges key by key.
func TestLoadConfigOverlay(t *testing.T) {
	hermetic(t)
	if got := loadConfig(); !reflect.DeepEqual(got, defaultConfig()) {
		t.Fatalf("no config.json: got %+v, want the embedded theme", got)
	}
	writeStatuslineConfig(t, `{"order":["model"],"profileMax":3}`)
	got, want := loadConfig(), defaultConfig()
	want.Order, want.ProfileMax = []string{"model"}, 3
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order/profileMax overlay: got %+v, want %+v", got, want)
	}
	writeStatuslineConfig(t, `{"segments":{"git":false}}`)
	wantSegs := maps.Clone(defaultConfig().Segments)
	wantSegs["git"] = false
	if got := loadConfig().Segments; !reflect.DeepEqual(got, wantSegs) {
		t.Fatalf("segments overlay = %v, want the embedded map with git off", got)
	}
	writeStatuslineConfig(t, `{"segments":{"path":false}}`)
	if got := loadConfig().Segments; got["proxy"] || got["session"] || !got["git"] || got["path"] {
		t.Fatalf("one-key overlay = %v, want embedded-false segments to stay false", got)
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
