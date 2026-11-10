package statusline

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/edotau/claude-code/internal/ui/theme"
)

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

func TestGradientPreservesOllamaTag(t *testing.T) {
	hermetic(t)
	writeStatuslineConfig(t, `{"segments":{"path":false,"git":false,"velocity":false,"proxy":false,"limits":false,"total":false}}`)
	got := plain(Render([]byte(`{"model":{"id":"qwen3:30b"}}`), "gradient"))
	if got != "qwen3:30b" {
		t.Fatalf("gradient model = %q, want qwen3:30b", got)
	}
}

func TestDashboardOmitsDisabledContext(t *testing.T) {
	hermetic(t)
	writeStatuslineConfig(t, `{"style":"dashboard","segments":{"context":false,"path":false,"git":false,"velocity":false,"proxy":false,"limits":false,"total":false}}`)
	got := plain(Render([]byte(`{"model":{"id":"qwen3:30b"},"context_window":{"used_percentage":75}}`), ""))
	if strings.Contains(got, "context") || strings.Contains(got, "%") {
		t.Fatalf("disabled context column rendered: %q", got)
	}
	if !strings.Contains(got, "qwen3:30b") {
		t.Fatalf("enabled model column missing: %q", got)
	}
}

func TestClipCountsWideCells(t *testing.T) {
	if got := visibleWidth("界界"); got != 4 {
		t.Errorf("wide text occupies %d columns, want 4", got)
	}
	if got := plain(clip("界界", 2)); got != "…" {
		t.Fatalf("clip wide text to 2 columns = %q, want ellipsis only", got)
	}
}

func TestTreeDiffDeadlineFallsBack(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STATUSLINE_GIT_MARKER", filepath.Join(dir, "listed"))
	script := "#!/bin/sh\nif test -f \"$STATUSLINE_GIT_MARKER\"; then\n exec sleep 5\nfi\n: > \"$STATUSLINE_GIT_MARKER\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	adds, dels, ok := treeDiffStat(dir)
	if ok || adds != 0 || dels != 0 {
		t.Fatalf("deadline expired: treeDiffStat = (%d, %d, %v), want (0, 0, false)", adds, dels, ok)
	}
}

func TestProfileMaxOneRune(t *testing.T) {
	hermetic(t)
	writeStatuslineConfig(t, `{"profileMax":1}`)
	if got := capProfile("anthropic·work", loadConfig().ProfileMax); got != "anthropic·…" {
		t.Fatalf("profileMax 1 = %q, want anthropic·…", got)
	}
}

type fullDiskWriter struct{}

func (fullDiskWriter) Write([]byte) (int, error) { return 0, syscall.ENOSPC }

func TestRunReportsWriteFailure(t *testing.T) {
	hermetic(t)
	writeStatuslineConfig(t, `{"segments":{"path":false,"git":false,"velocity":false,"proxy":false,"limits":false,"total":false}}`)
	fs := flag.NewFlagSet("statusline", flag.ContinueOnError)
	if code := Run(fs, nil, strings.NewReader(`{"model":{"id":"qwen3:30b"}}`), fullDiskWriter{}); code != 1 {
		t.Fatalf("Run with ENOSPC = %d, want 1", code)
	}
}

func TestConfiguredGitRendersOnce(t *testing.T) {
	hermetic(t)
	cfg := embeddedConfig()
	cfg.Style, cfg.Order = "minimal", []string{"model", "git"}
	cfg.Segments = map[string]bool{"path": false}
	got := plain(render(values{model: "qwen3:30b", branch: "feature-once"}, cfg, theme.EmbeddedPalettes()))
	if count := strings.Count(got, "feature-once"); count != 1 {
		t.Fatalf("branch rendered %d times, want 1: %q", count, got)
	}
}

func TestInvalidConfigIgnoredWhole(t *testing.T) {
	hermetic(t)
	writeStatuslineConfig(t, `{"style":"minimal","width":"invalid"}`)
	if got, want := loadConfig(), embeddedConfig(); !reflect.DeepEqual(got, want) {
		t.Fatalf("invalid config partially applied: got %+v, want %+v", got, want)
	}
}

func TestDashboardPercentBeforeBar(t *testing.T) {
	hermetic(t)
	writeStatuslineConfig(t, `{"style":"dashboard","segments":{"path":false},"dashboard":{"headers":false},"bar":{"width":4,"filledGlyph":"#","emptyGlyph":".","percent":"before"}}`)
	cfg := loadConfig()
	cfg.Dashboard.Columns = []string{"context"}
	got := strings.TrimSpace(plain(render(values{contextPct: 75}, cfg, theme.EmbeddedPalettes())))
	if got != "75% ###." {
		t.Fatalf("dashboard percent before bar = %q, want 75%% ###.", got)
	}
}

func TestGradientClipsOversizedSegment(t *testing.T) {
	hermetic(t)
	cfg := embeddedConfig()
	cfg.Style, cfg.Width, cfg.Order = "gradient", 12, []string{"model"}
	cfg.Segments = map[string]bool{"path": false}
	got := plain(renderFit(values{model: strings.Repeat("m", 30)}, cfg, theme.EmbeddedPalettes(), cfg.Width))
	if got != strings.Repeat("m", 11)+"…" {
		t.Fatalf("gradient width 12 = %q, want 11 model cells and ellipsis", got)
	}
}
