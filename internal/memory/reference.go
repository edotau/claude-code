package memory

import (
	"embed"
	"fmt"
	"strings"
)

//go:embed prompts/*.md
var promptsFS embed.FS

// Reference loads the model-residue prompt for verb from the embedded prompts/<verb>.md — the one
// copy of each procedure; a background job and its CLI verb print the same text.
// A missing or empty file is an error: a job must never run on a silently empty prompt.
func Reference(verb string) (string, error) {
	data, err := promptsFS.ReadFile("prompts/" + verb + ".md")
	if err != nil {
		return "", fmt.Errorf("memory reference %s: %w", verb, err)
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return "", fmt.Errorf("memory reference %s: prompts/%s.md is empty", verb, verb)
	}
	return text, nil
}
