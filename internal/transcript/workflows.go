package transcript

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// WorkflowRunDirs lists one session's subagents/workflows/<runId> dirs.
func WorkflowRunDirs(sessionDir string) []string {
	matches, err := filepath.Glob(filepath.Join(sessionDir, "subagents", "workflows", "*"))
	if err != nil {
		return nil
	}
	return KeepDirs(matches)
}

// KeepDirs filters a glob result down to directories, sorted.
func KeepDirs(matches []string) []string {
	var dirs []string
	for _, m := range matches {
		if info, err := os.Stat(m); err == nil && info.IsDir() {
			dirs = append(dirs, m)
		}
	}
	slices.Sort(dirs)
	return dirs
}

// WorkflowAgentFiles lists one run dir's agent transcripts; the agent-*.jsonl shape keeps the sibling
// *.meta.json sidecars out of the token scan.
func WorkflowAgentFiles(runDir string) []string {
	matches, err := filepath.Glob(filepath.Join(runDir, "agent-*.jsonl"))
	if err != nil {
		return nil
	}
	slices.Sort(matches)
	return matches
}

// WorkflowPhaseFromMeta reads the sibling agent-*.meta.json's workflowPhase; plenty of real metas
// carry none, so "" is a normal answer routed to `(none)` .
func WorkflowPhaseFromMeta(transcriptPath string, errw io.Writer) string {
	raw, err := os.ReadFile(strings.TrimSuffix(transcriptPath, ".jsonl") + ".meta.json")
	if err != nil {
		return ""
	}
	var meta struct {
		WorkflowPhase string `json:"workflowPhase"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		fmt.Fprintf(errw, "usage: warning: skipping meta for %s: %v\n", transcriptPath, err)
		return ""
	}
	return meta.WorkflowPhase
}
