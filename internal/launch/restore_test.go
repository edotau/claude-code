package launch

import (
	"os"
	"path/filepath"
	"testing"
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

func TestRestoreSettingsOutputReportsStdoutFailure(t *testing.T) {
	for _, tt := range []struct {
		name   string
		out    string
		dryRun bool
	}{
		{name: "stdout"},
		{name: "dry run", out: "not-written.json", dryRun: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := restoreSettingsOutput(closedStdout(t), tt.out, false, tt.dryRun, []byte("{}\n")); err == nil {
				t.Fatal("restoreSettingsOutput on closed stdout returned nil error")
			}
		})
	}
}
