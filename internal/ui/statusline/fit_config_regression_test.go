package statusline

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/edotau/claude-code/internal/ui/theme"
)

func TestRenderFitWrapsWideSegments(t *testing.T) {
	hermetic(t)
	for _, tc := range []struct {
		name  string
		v     values
		width int
		want  string
	}{
		{"path", values{userHost: "u", path: "界界", model: "tail"}, 11, "u:界界\ntail"},
		{"hostname", values{userHost: "界界", path: "p", model: "tail"}, 11, "界界:p\ntail"},
		{"next segment", values{userHost: "u", path: "p", model: "界界"}, 9, "u:p\n界界"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.Style, cfg.Width, cfg.Order = "gradient", tc.width, []string{"model"}
			got := plain(renderFit(tc.v, cfg, theme.EmbeddedPalettes(), tc.width))
			if got != tc.want {
				t.Fatalf("wide segment wrapping = %q, want %q", got, tc.want)
			}
		})
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
