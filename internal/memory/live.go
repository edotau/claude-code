package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/memory/recall"
	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/transcript"
)

// SessionIDEnv is the env var Claude Code sets in every live session's tools.
const SessionIDEnv = "CLAUDE_CODE_SESSION_ID"

// sessionRoot is the transcript's recorded SessionStart root, else its project's cwd; "" if neither is known.
func sessionRoot(path string) string {
	if root := recall.SessionRoot(strings.TrimSuffix(filepath.Base(path), ".jsonl"), ""); root != "" {
		return root
	}
	return projectCWD(path)
}

// projectCWD is the first cwd whose slug names the transcript's project dir; a session's first cwd may be elsewhere.
func projectCWD(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	slug, found := filepath.Base(filepath.Dir(path)), ""
	transcript.EachLine(f, func(line []byte) {
		var rec struct {
			CWD string `json:"cwd"`
		}
		if found == "" && json.Unmarshal(line, &rec) == nil && rec.CWD != "" && memstore.ProjectSlug(rec.CWD) == slug {
			found = rec.CWD
		}
	})
	return found
}

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
	if p := filepath.Join(projects, memstore.ProjectSlug(cwd), id); paths.IsFile(p) {
		return p
	}
	if m, _ := filepath.Glob(filepath.Join(projects, "*", id)); len(m) > 0 {
		return m[0]
	}
	return ""
}
