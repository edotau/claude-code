package transcript

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
)

// AgentIDs lists subagent ids from cwd's project's most recent sessions — a bounded glob, current session first.
func AgentIDs(cwd string) []string {
	projectDir := filepath.Join(paths.ConfigDir(), "projects", memstore.ProjectSlug(cwd))
	sessions, err := filepath.Glob(filepath.Join(projectDir, "*.jsonl"))
	if err != nil || len(sessions) == 0 {
		return nil
	}
	sortNewestFirst(sessions)
	const maxSessions = 2
	if len(sessions) > maxSessions {
		sessions = sessions[:maxSessions]
	}
	seen := map[string]bool{}
	var ids []string
	for _, s := range sessions {
		dir := strings.TrimSuffix(s, ".jsonl")
		for _, pattern := range []string{
			filepath.Join(dir, "subagents", "agent-*.jsonl"),
			filepath.Join(dir, "subagents", "workflows", "*", "agent-*.jsonl"),
		} {
			matches, _ := filepath.Glob(pattern)
			ids = appendNewIDs(ids, seen, matches)
		}
	}
	return ids
}

// sortNewestFirst orders sessions by mtime, newest first, stat-ing each file once.
func sortNewestFirst(sessions []string) {
	modTime := make(map[string]time.Time, len(sessions)) // stat each session once, not once per comparison
	for _, s := range sessions {
		if fi, err := os.Stat(s); err == nil {
			modTime[s] = fi.ModTime()
		}
	}
	slices.SortFunc(sessions, func(a, b string) int {
		at, aok := modTime[a]
		bt, bok := modTime[b]
		if !aok || !bok {
			return 0
		}
		return bt.Compare(at)
	})
}

// appendNewIDs adds the agent id of each matched transcript file that seen does not hold yet.
func appendNewIDs(ids []string, seen map[string]bool, matches []string) []string {
	for _, m := range matches {
		id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "agent-"), ".jsonl")
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}
