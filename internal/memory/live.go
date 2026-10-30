package memory

import (
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// SessionIDEnv is the env var Claude Code sets in every live session's tools.
const SessionIDEnv = "CLAUDE_CODE_SESSION_ID"

// LiveTranscript resolves the running session's transcript: <id>.jsonl under projects/<cwd-slug>, else any project.
// It returns "" when sessionID is blank or no file exists, so a bare `memory update` outside a session stays a no-op.
func LiveTranscript(cwd, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	id := filepath.Base(sessionID)
	if !strings.HasSuffix(id, ".jsonl") {
		id += ".jsonl"
	}
	projects := filepath.Join(paths.ConfigDir(), "projects")
	if p := filepath.Join(projects, ProjectSlug(cwd), id); paths.IsFile(p) {
		return p
	}
	if m, _ := filepath.Glob(filepath.Join(projects, "*", id)); len(m) > 0 {
		return m[0]
	}
	return ""
}
