package agent

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/proc"
)

// regEntry is one ~/.claude/sessions/<pid>.json: Claude Code's own record of a running CLI process.
type regEntry struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Name      string `json:"name"`
	Status    string `json:"status"` // idle|busy|waiting
	Kind      string `json:"kind"`   // interactive|bg; Claude Code's own key, surfaced as Session.Mode
}

// pidAlive is a var so tests can stub the liveness probe.
var pidAlive = proc.Alive

// readRegistry returns the live sessions/*.json entries keyed by session id; unreadable or dead ones are skipped.
func readRegistry(claudeDir string) map[string]regEntry {
	out := map[string]regEntry{}
	files, _ := filepath.Glob(filepath.Join(claudeDir, "sessions", "*.json"))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var e regEntry
		if json.Unmarshal(data, &e) != nil || e.PID <= 0 || e.SessionID == "" || !pidAlive(e.PID) {
			continue
		}
		out[e.SessionID] = e
	}
	return out
}

// applyRegistry copies a live registry entry onto s; a busy CLI is active, any other live state idle.
func applyRegistry(s Session, e regEntry) Session {
	s.Name, s.PID, s.Live, s.CLIStatus, s.Mode = e.Name, e.PID, true, e.Status, e.Kind
	if s.Cwd == "" {
		s.Cwd = e.Cwd
	}
	s.Status = statusIdle
	if e.Status == "busy" {
		s.Status = statusActive
	}
	return s
}

// liveTranscripts finds projects/*/<id>.jsonl for each registry entry not already in have.
func liveTranscripts(claudeDir string, reg map[string]regEntry, have map[string]bool) []string {
	var files []string
	for id := range reg {
		if have[id] || id != filepath.Base(id) || id == ".." {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(claudeDir, "projects", "*", id+".jsonl"))
		files = append(files, matches...)
	}
	return files
}
