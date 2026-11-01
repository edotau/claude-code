package paths

import (
	"os"
	"path/filepath"
)

func CodexConfigPath(home string) (string, error) {
	if home != "" {
		return filepath.Abs(filepath.Join(home, ".codex", "config.toml"))
	}
	dir, err := CodexHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.toml"), nil
}

// CodexHome is $CODEX_HOME (made absolute) or ~/.codex, the directory holding codex's config.toml and auth.json.
func CodexHome() (string, error) {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return filepath.Abs(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}
