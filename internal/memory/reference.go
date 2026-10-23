package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// Reference loads the model-residue prompt for verb from skills/reflection/references/<verb>.md —
// the one copy of each procedure; a background job and its slash command read the same file.
// A missing or empty file is an error: a job must never run on a silently empty prompt.
func Reference(verb string) (string, error) {
	p := filepath.Join(paths.ConfigDir(), "skills", "reflection", "references", verb+".md")
	data, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("memory reference %s: %w", verb, err)
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return "", fmt.Errorf("memory reference %s: %s is empty", verb, p)
	}
	return text, nil
}
