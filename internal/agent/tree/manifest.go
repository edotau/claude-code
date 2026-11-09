package tree

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// subagentMeta is the subset of agent-<id>.meta.json this view renders.
type subagentMeta struct {
	AgentType   string `json:"agentType"`
	Description string `json:"description"`
	SpawnDepth  int    `json:"spawnDepth"`
	Finished    bool   `json:"finished"`
	Model       string `json:"model"`
}

// readSubagentMeta reads the sidecar .meta.json beside transcriptPath; a missing or bad one yields a zero meta.
func readSubagentMeta(transcriptPath string) subagentMeta {
	raw, err := os.ReadFile(strings.TrimSuffix(transcriptPath, ".jsonl") + ".meta.json")
	if err != nil {
		return subagentMeta{}
	}
	var m subagentMeta
	if json.Unmarshal(raw, &m) != nil { // a partial decode would keep fields set before the bad one
		return subagentMeta{}
	}
	return m
}

// workflowManifest is a workflows/wf_<id>.json run record; Result stays RawMessage (string, object or null).
type workflowManifest struct {
	RunID     string          `json:"runId"`
	Timestamp string          `json:"timestamp"`
	Script    string          `json:"script"`
	Result    json.RawMessage `json:"result"`
	Status    string          `json:"status"` // the run's own completed|killed, present on every manifest
}

// resultStatus reads result.status, else the manifest status (completed -> finished, killed -> error); "" otherwise.
func (m *workflowManifest) resultStatus() string {
	var r struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(m.Result, &r) == nil && r.Status != "" {
		return r.Status
	}
	switch m.Status {
	case "killed":
		return StatusError
	case "completed":
		return StatusFinished
	}
	return ""
}

// manifestNamePattern pulls the name out of the script's `meta = { name: '...' }`, never another object's name.
var manifestNamePattern = regexp.MustCompile(`\bmeta\s*=\s*\{[^}]*?\bname:\s*'([^']+)'`)

// name resolves the manifest's display name from its script's meta.name.
func (m *workflowManifest) name() string {
	if match := manifestNamePattern.FindStringSubmatch(m.Script); len(match) == 2 {
		return match[1]
	}
	return ""
}

// workflowManifests reads every <sessionDir>/workflows/wf_*.json manifest, keyed by runId.
func workflowManifests(sessionDir string) map[string]*workflowManifest {
	out := map[string]*workflowManifest{}
	matches, err := filepath.Glob(filepath.Join(sessionDir, "workflows", "wf_*.json"))
	if err != nil {
		return out
	}
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var m workflowManifest
		if json.Unmarshal(raw, &m) != nil || m.RunID == "" {
			continue
		}
		out[m.RunID] = &m
	}
	return out
}

// journalEntry records lifecycle state and a result preview; a null result marks a killed agent.
type journalEntry struct {
	Label       string
	Started     bool
	Resulted    bool
	Errored     bool
	Result      string
	ResultBytes int
}

// journalLine is one journal.jsonl record; Result stays RawMessage so a JSON null differs from an absent field.
type journalLine struct {
	Type    string          `json:"type"`
	AgentID string          `json:"agentId"`
	Label   string          `json:"label"`
	Result  json.RawMessage `json:"result"`
}

// readJournal reads a run's journal.jsonl into one entry per agentId; a missing or bad journal gives an empty map.
func readJournal(runDir string) map[string]journalEntry {
	out := map[string]journalEntry{}
	eachLine(filepath.Join(runDir, "journal.jsonl"), func(raw []byte) {
		var line journalLine
		if json.Unmarshal(raw, &line) != nil || line.AgentID == "" {
			return
		}
		e := out[line.AgentID]
		switch line.Type {
		case "started":
			e.Started, e.Label = true, line.Label
		case "result":
			e.Resulted = true
			if isJSONNull(line.Result) {
				e.Errored = true
			} else {
				e.Result = resultPreview(line.Result)
			}
			e.ResultBytes = len(bytes.TrimSpace(line.Result))
		}
		out[line.AgentID] = e
	})
	return out
}
