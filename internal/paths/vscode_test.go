package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestVSCodeSettingsIsPlatformSpecific(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Skip("HOME-based home dir override does not apply on windows")
	}

	dest, err := VSCodeSettings()
	if err != nil {
		t.Fatal(err)
	}
	switch runtime.GOOS {
	case "darwin":
		want := filepath.Join(home, "Library", "Application Support", "Code", "User", "settings.json")
		if dest != want {
			t.Errorf("dest = %s, want %s", dest, want)
		}
	default:
		// No vscode-server dir yet: falls back to the desktop (User scope) path.
		want := filepath.Join(home, ".config", "Code", "User", "settings.json")
		if dest != want {
			t.Errorf("dest (no vscode-server) = %s, want %s", dest, want)
		}
		if err := os.MkdirAll(filepath.Join(home, ".vscode-server"), 0o755); err != nil {
			t.Fatal(err)
		}
		dest, err = VSCodeSettings()
		if err != nil {
			t.Fatal(err)
		}
		want = filepath.Join(home, ".vscode-server", "data", "Machine", "settings.json")
		if dest != want {
			t.Errorf("dest (with vscode-server) = %s, want %s", dest, want)
		}
	}
}
