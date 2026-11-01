package paths

import (
	"os"
	"path/filepath"
)

// CopilotHome is $COPILOT_HOME unchanged, or ~/.copilot when the override is empty.
func CopilotHome() (string, error) {
	if dir := os.Getenv("COPILOT_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".copilot"), nil
}
