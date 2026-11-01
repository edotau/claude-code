package launch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/settings"
)

// closedStdout returns a closed file, so every write to it fails.
func closedStdout(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "closed-stdout"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestWriteSettingsOutputReportsStdoutFailure(t *testing.T) {
	for _, tt := range []struct {
		name   string
		out    string
		dryRun bool
	}{
		{name: "stdout"},
		{name: "dry run", out: "not-written.json", dryRun: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := writeSettingsOutput(closedStdout(t), tt.out, false, tt.dryRun, []byte("{}\n")); err == nil {
				t.Fatal("writeSettingsOutput on closed stdout returned nil error")
			}
		})
	}
	t.Run("check does not write", func(t *testing.T) {
		// Hermetic: a clean render in a temp config dir, never the user's live settings.json.
		dir := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", dir)
		rendered, err := settings.RenderLive()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), rendered, 0o600); err != nil {
			t.Fatal(err)
		}
		if drift, err := writeSettingsOutput(closedStdout(t), "", true, false, rendered); drift || err != nil {
			t.Fatalf("check-only output = %v, %v, want false, nil", drift, err)
		}
	})
}
