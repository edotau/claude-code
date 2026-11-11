package tree

import (
	"cmp"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/transcript"
)

// subagentMeta is the subset of agent-<id>.meta.json this view renders.
type subagentMeta struct {
	AgentType   string `json:"agentType"`
	Description string `json:"description"`
	SpawnDepth  int    `json:"spawnDepth"`
	Finished    bool   `json:"finished"`
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
		return statusError
	case "completed":
		return statusFinished
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

// journalEntry records lifecycle state; a null result marks a killed agent.
type journalEntry struct {
	Label    string
	Started  bool
	Resulted bool
	Errored  bool
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
	f, err := os.Open(filepath.Join(runDir, "journal.jsonl"))
	if err != nil {
		return out
	}
	defer f.Close()
	_ = transcript.EachLine(f, func(raw []byte) {
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
			e.Errored = e.Errored || isJSONNull(line.Result)
		}
		out[line.AgentID] = e
	})
	return out
}

// buildWorkflowRuns folds wf_*.json manifests and their subagents/workflows/<runId> transcripts by workflowPhase.
func buildWorkflowRuns(sessionDir string, now time.Time, cwd string) []WorkflowRun {
	manifests := workflowManifests(sessionDir)
	runDirs := transcript.WorkflowRunDirs(sessionDir)
	seen := map[string]bool{}
	var runs []WorkflowRun

	addRun := func(runID string) {
		if seen[runID] {
			return
		}
		seen[runID] = true
		runs = append(runs, buildOneWorkflowRun(sessionDir, runID, manifests[runID], now, cwd))
	}
	for id := range manifests {
		addRun(id)
	}
	for _, dir := range runDirs {
		addRun(filepath.Base(dir))
	}
	slices.SortStableFunc(runs, func(a, b WorkflowRun) int {
		if c := b.Updated.Compare(a.Updated); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return runs
}

// buildOneWorkflowRun folds a run manifest (maybe absent) with its transcripts by phase; the journal sets status.
func buildOneWorkflowRun(sessionDir, runID string, m *workflowManifest, now time.Time, cwd string) WorkflowRun {
	run := WorkflowRun{ID: runID, Name: runID}
	if m != nil {
		run.Name = cmp.Or(m.name(), runID)
		run.Updated = parseRFC3339(m.Timestamp)
	}

	runDir := filepath.Join(sessionDir, "subagents", "workflows", runID)
	journal := readJournal(runDir)
	files := transcript.WorkflowAgentFiles(runDir)
	byPhase := map[string]*WorkflowPhase{}
	var order []string
	for _, f := range files {
		sub := buildSubagent(f, now, cwd)
		sub = applyJournalEntry(sub, journal[sub.ID])
		phaseName := cmp.Or(transcript.WorkflowPhaseFromMeta(f, io.Discard), "(none)")
		phase := byPhase[phaseName]
		if phase == nil {
			phase = &WorkflowPhase{Name: phaseName}
			byPhase[phaseName] = phase
			order = append(order, phaseName)
		}
		phase.Agents = append(phase.Agents, sub)
		if sub.Updated.After(run.Updated) {
			run.Updated = sub.Updated
		}
	}
	for _, name := range order {
		run.Phases = append(run.Phases, *byPhase[name])
	}
	run.Status = workflowRunStatus(m, journal, run.Updated, now)
	return run
}

// applyJournalEntry overrides status/description from the journal: a result wins (error if null); zero is a no-op.
func applyJournalEntry(sub Subagent, j journalEntry) Subagent {
	if sub.Description == "" && j.Label != "" {
		sub.Description = j.Label
	}
	if j.Resulted {
		sub.Status = statusFinished
		if j.Errored {
			sub.Status = statusError
		}
	}
	return sub
}

// workflowRunStatus prefers the manifest result status, else finished once every started agent has a result, quiet.
func workflowRunStatus(m *workflowManifest, journal map[string]journalEntry, updated, now time.Time) string {
	if m != nil {
		if s := m.resultStatus(); s != "" {
			return s
		}
	}
	started, resulted := 0, 0
	for _, j := range journal {
		if j.Started {
			started++
		}
		if j.Resulted {
			resulted++
		}
	}
	if started > 0 && started == resulted && now.Sub(updated) > subagentActiveWindow {
		return statusFinished
	}
	return statusRunning
}
