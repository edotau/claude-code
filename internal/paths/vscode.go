package paths

import (
	"os"
	"path/filepath"
	"runtime"
)

// VSCodeSettings is the platform's VS Code Machine settings path; outside a vscode-server it is the User scope.
func VSCodeSettings() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Code", "User", "settings.json"), nil
	case "windows":
		return filepath.Join(home, "AppData", "Roaming", "Code", "User", "settings.json"), nil
	default:
		if info, statErr := os.Stat(filepath.Join(home, ".vscode-server")); statErr == nil && info.IsDir() {
			return filepath.Join(home, ".vscode-server", "data", "Machine", "settings.json"), nil
		}
		return filepath.Join(home, ".config", "Code", "User", "settings.json"), nil
	}
}
