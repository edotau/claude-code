package tree

import (
	"cmp"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/transcript"
)

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
		sub.Status = StatusFinished
		if j.Errored {
			sub.Status = StatusError
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
		return StatusFinished
	}
	return StatusRunning
}
