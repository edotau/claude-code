package gemini

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/agent"
)

// geminiLogin runs agy on its own cached Google sign-in (AgyLoginMarker); without one it errors with the sign-in hint.
func geminiLogin(model string, h *agent.Headless) ([]string, []string, error) {
	present, perr := geminiLoginProbe()
	if perr != nil {
		return nil, nil, fmt.Errorf("gemini: agy login marker unreadable: %w", perr)
	}
	if !present {
		s, _ := agent.LookupSchema("gemini")
		return nil, nil, errors.New("gemini: agy is not signed in; " + s.CLI.ConfigureHint)
	}
	if h != nil {
		return nil, nil, nil // headless argv is runAgy's (prompt over stdin stream-json)
	}
	return nil, []string{"--model", model}, nil
}

// geminiLoginProbe reports whether agy's cached Google login is on disk, plus the first real read failure.
func geminiLoginProbe() (bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	for _, p := range agyLoginMarkers(home) {
		fi, serr := os.Stat(p)
		if serr == nil && fi.Size() > 0 {
			return true, nil
		}
		if serr != nil && !errors.Is(serr, fs.ErrNotExist) && err == nil {
			err = serr
		}
	}
	return false, err
}

// AgyLoginMarker is agy's cached Google OAuth token path under home, the marker a login run leaves behind.
func AgyLoginMarker(home string) string {
	return agyLoginMarkers(home)[0]
}

// agyLoginMarkers lists every path agy has used for its cached Google OAuth token, newest first.
func agyLoginMarkers(home string) []string {
	return []string{
		filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token"),
		filepath.Join(home, ".gemini", "jetski-standalone-oauth-token"),
	}
}
