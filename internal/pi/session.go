package pi

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// sessionDir is where Pi saves the sessions that --session resumes.
func sessionDir() string { return filepath.Join(paths.StateDir(), "pi", "sessions") }

// sessionFile is the saved session file named for id under dir, or "".
func sessionFile(dir, id string) string {
	found := ""
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && found == "" && !d.IsDir() && strings.Contains(d.Name(), id) && strings.HasSuffix(d.Name(), ".jsonl") {
			found = p
		}
		return nil
	})
	return found
}

// sessionSaved reports whether Pi wrote a session file named for id under dir.
func sessionSaved(dir, id string) bool { return sessionFile(dir, id) != "" }

// sessionCwd is the directory a saved session was started in; Pi prompts to fork it from any other directory.
func sessionCwd(dir, id string) string {
	if id == "" {
		return ""
	}
	f, err := os.Open(sessionFile(dir, id))
	if err != nil {
		return ""
	}
	defer f.Close()
	var hdr struct{ Cwd string }
	if json.NewDecoder(f).Decode(&hdr) != nil {
		return ""
	}
	if st, err := os.Stat(hdr.Cwd); err != nil || !st.IsDir() {
		return ""
	}
	return hdr.Cwd
}
