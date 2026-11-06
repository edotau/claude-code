package theme

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"

	"github.com/edotau/claude-code/internal/ui/ansi"
)

func near(a, b ansi.RGB) bool {
	for i := range a {
		if d := a[i] - b[i]; d < -1 || d > 1 {
			return false
		}
	}
	return true
}

func TestLiftIdentityAtZero(t *testing.T) {
	in := ansi.Hex("#7C6C7C")
	if got := (Look{}).Lifted(in); got != in {
		t.Fatalf("lift 0 changed %v to %v", in, got)
	}
	pal := EmbeddedPalettes()
	if got := (Look{}).LiftPalette(pal); got.Sem("dim") != pal.Sem("dim") {
		t.Fatalf("lift 0 changed dim: %s", got.Sem("dim"))
	}
}

func TestLiftKnownValues(t *testing.T) {
	c := Look{Lift: 0.15}
	for hex, want := range map[string]string{"#7C6C7C": "#908291", "#4796E4": "#62A6E8"} {
		if got := c.Lifted(ansi.Hex(hex)); !near(got, ansi.Hex(want)) {
			t.Errorf("%s: got %v want %v", hex, got, ansi.Hex(want))
		}
	}
	pal := EmbeddedPalettes()
	if got := c.LiftPalette(pal).Sem("dim"); !near(ansi.Hex(got), c.Lifted(ansi.Hex(pal.Sem("dim")))) {
		t.Errorf("palette dim = %s", got)
	}
}

func TestLiftGeminiStops(t *testing.T) {
	base := Look{Gradient: "gemini"}.Stops(EmbeddedPalettes())
	got := Look{Gradient: "gemini", Lift: 0.15}.Stops(EmbeddedPalettes())
	for i := range base {
		for ch := range base[i] {
			if got[i][ch] < base[i][ch] || got[i][ch] > 255 {
				t.Fatalf("stop %d ch %d: %d -> %d", i, ch, base[i][ch], got[i][ch])
			}
		}
		if (base[i][0] > base[i][1]) != (got[i][0] > got[i][1]) && base[i][0] != base[i][1] {
			t.Errorf("stop %d channel order flipped: %v -> %v", i, base[i], got[i])
		}
	}
}

func TestLiftClamped(t *testing.T) {
	in := ansi.Hex("#4796E4")
	if got, want := (Look{Lift: 5}).Lifted(in), (Look{Lift: 0.6}).Lifted(in); got != want {
		t.Errorf("high not clamped: %v vs %v", got, want)
	}
	if got := (Look{Lift: -1}).Lifted(in); got != in {
		t.Errorf("negative not clamped: %v", got)
	}
}

func writeUserConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	path := filepath.Join(dir, "state", "statusline", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// A config whose later field has the wrong type must not leave the earlier "lift" half-applied.
func TestThemeMalformedConfigKeepsDefaultLook(t *testing.T) {
	wantMode, wantStops, _ := Theme()
	writeUserConfig(t, `{"lift": 0.6, "gradient": 5}`)
	mode, stops, _ := Theme()
	if mode != wantMode || len(stops) != len(wantStops) {
		t.Fatalf("mode/len = %s/%d, want %s/%d", mode, len(stops), wantMode, len(wantStops))
	}
	for i := range stops {
		if stops[i] != wantStops[i] {
			t.Errorf("stop %d = %v, want default %v (lift half-applied)", i, stops[i], wantStops[i])
		}
	}
}

// SetGradient backs up an existing config.json before rewriting it.
func TestSetGradientBacksUpExistingConfig(t *testing.T) {
	path := writeUserConfig(t, `{"gradient":"gemini","lift":0.2}`)
	if err := SetGradient("ocean"); err != nil {
		t.Fatal(err)
	}
	pattern := paths.BackupPath(path, time.Now())
	matches, _ := filepath.Glob(pattern[:len(pattern)-len("20060102-150405.000000000")] + "*")
	if len(matches) != 1 {
		t.Fatalf("backups = %v, want exactly one", matches)
	}
	got, err := os.ReadFile(matches[0])
	if err != nil || string(got) != `{"gradient":"gemini","lift":0.2}` {
		t.Errorf("backup = %q, %v", got, err)
	}
}
