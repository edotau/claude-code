// Package agenttree builds `claude-code agents tree`: every recent session across every project,
// its subagents (and the Workflow runs/phases inside it), read straight off ~/.claude/projects.
package agenttree

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/transcript"
	"github.com/edotau/claude-code/internal/usage"
)

// activeWindow is how recently a transcript must have been written to count as "running"/"active";
// past it but unfinished is "stale".
const activeWindow = 10 * time.Minute

// Tokens is a compact input/output token pair, used for both a session and a subagent.
type Tokens struct {
	In  int64 `json:"in"`
	Out int64 `json:"out"`
}

// Subagent is one subagents/agent-<id>.jsonl + its sibling .meta.json.
type Subagent struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	Description string    `json:"description"`
	Status      string    `json:"status"` // running|finished|stale
	Depth       int       `json:"depth"`
	Tokens      Tokens    `json:"tokens"`
	LastTool    string    `json:"lastTool"`
	Updated     time.Time `json:"updated"`
}

// WorkflowPhase folds a run's agents under the workflowPhase their meta.json names ("(none)" when absent).
type WorkflowPhase struct {
	Name   string     `json:"name"`
	Agents []Subagent `json:"agents"`
}

// WorkflowRun is one workflows/wf_<id>.json manifest plus the matching
// subagents/workflows/<runId>/agent-*.jsonl transcripts folded into phases.
type WorkflowRun struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Status  string          `json:"status"` // the manifest's result.status ("" when still running, no manifest yet)
	Updated time.Time       `json:"updated"`
	Phases  []WorkflowPhase `json:"phases"`
}

// Session is one projects/<slug>/<session>.jsonl plus everything nested under
// projects/<slug>/<session>/.
type Session struct {
	ID        string        `json:"id"`
	Project   string        `json:"project"` // the on-disk project slug (sanitized cwd)
	Cwd       string        `json:"cwd"`
	Branch    string        `json:"branch"`
	Title     string        `json:"title"`
	Updated   time.Time     `json:"updated"`
	Status    string        `json:"status"` // active|idle
	Tokens    Tokens        `json:"tokens"`
	Subagents []Subagent    `json:"subagents"`
	Workflows []WorkflowRun `json:"workflows,omitempty"`
}

// Tree is the stable `agents tree --format json` schema: {sessions:[{id, project, cwd, branch,
// title, updated, status, tokens, subagents:[{id, type, description, status, depth, tokens,
// lastTool, updated}], workflows:[{id, name, status, updated, phases:[{name, agents:[...]}]}]}]}.
type Tree struct {
	Sessions []Session `json:"sessions"`
}

// Options scopes a Build: Days bounds by session-dir mtime (ignored when Session is set), Session
// narrows to one session id across every project dir, Running keeps only sessions/subagents not
// finished. Now defaults to time.Now when nil — tests pin it so running/stale classification never
// depends on wall-clock time.
type Options struct {
	Days    int
	Session string
	Running bool
	Now     func() time.Time
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Build scans claudeDir/projects for every session in scope, newest first.
func Build(claudeDir string, opts Options) (*Tree, error) {
	now := opts.now()
	sessionFiles, err := sessionTranscriptsInScope(claudeDir, opts, now)
	if err != nil {
		return nil, err
	}
	tree := &Tree{}
	for _, f := range sessionFiles {
		s := buildSession(f, now)
		if opts.Running && !sessionHasActivity(s) {
			continue
		}
		tree.Sessions = append(tree.Sessions, s)
	}
	slices.SortStableFunc(tree.Sessions, func(a, b Session) int { return b.Updated.Compare(a.Updated) })
	return tree, nil
}

// sessionHasActivity reports whether --running should keep this session: its own status is active,
// or at least one subagent (direct or inside a workflow run) is still running.
func sessionHasActivity(s Session) bool {
	if s.Status == statusActive {
		return true
	}
	for _, sub := range s.Subagents {
		if sub.Status == statusRunning {
			return true
		}
	}
	for _, run := range s.Workflows {
		for _, phase := range run.Phases {
			for _, sub := range phase.Agents {
				if sub.Status == statusRunning {
					return true
				}
			}
		}
	}
	return false
}

const (
	statusActive   = "active"
	statusIdle     = "idle"
	statusRunning  = "running"
	statusFinished = "finished"
	statusStale    = "stale"
)

// sessionTranscriptsInScope lists the projects/*/<session>.jsonl files in scope: every project dir
// for --session, else every one modified within --days.
func sessionTranscriptsInScope(claudeDir string, opts Options, now time.Time) ([]string, error) {
	if opts.Session != "" {
		pattern := filepath.Join(claudeDir, "projects", "*", opts.Session+".jsonl")
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		return matches, nil
	}
	days := opts.Days
	if days <= 0 {
		days = 1
	}
	pattern := filepath.Join(claudeDir, "projects", "*", "*.jsonl")
	candidates, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	var files []string
	for _, f := range candidates {
		info, statErr := os.Stat(f)
		if statErr != nil || info.ModTime().Before(cutoff) {
			continue
		}
		files = append(files, f)
	}
	slices.Sort(files)
	return files, nil
}

// buildSession reads one session's main transcript, its subagents dir and its workflow runs.
func buildSession(transcriptPath string, now time.Time) Session {
	id := strings.TrimSuffix(filepath.Base(transcriptPath), ".jsonl")
	sessionDir := strings.TrimSuffix(transcriptPath, ".jsonl")
	project := filepath.Base(filepath.Dir(transcriptPath))

	info, _ := os.Stat(transcriptPath)
	var updated time.Time
	if info != nil {
		updated = info.ModTime()
	}

	head := scanSessionHead(transcriptPath)
	s := Session{
		ID:      id,
		Project: project,
		Cwd:     head.cwd,
		Branch:  head.branch,
		Title:   head.title,
		Updated: updated,
		Status:  sessionStatus(updated, now),
	}

	for _, agentFile := range subagentFiles(sessionDir) {
		sub := buildSubagent(agentFile, now)
		s.Tokens.In += sub.Tokens.In
		s.Tokens.Out += sub.Tokens.Out
		s.Subagents = append(s.Subagents, sub)
	}
	slices.SortStableFunc(s.Subagents, func(a, b Subagent) int { return b.Updated.Compare(a.Updated) })

	for _, run := range buildWorkflowRuns(sessionDir, now) {
		for _, phase := range run.Phases {
			for _, sub := range phase.Agents {
				s.Tokens.In += sub.Tokens.In
				s.Tokens.Out += sub.Tokens.Out
			}
		}
		s.Workflows = append(s.Workflows, run)
	}
	return s
}

// sessionStatus: active when the transcript was written within activeWindow of now, else idle.
func sessionStatus(updated, now time.Time) string {
	if !updated.IsZero() && now.Sub(updated) <= activeWindow {
		return statusActive
	}
	return statusIdle
}

// subagentFiles lists <sessionDir>/subagents/agent-*.jsonl, excluding the workflows/ subtree (those
// are folded into Workflows instead).
func subagentFiles(sessionDir string) []string {
	matches, err := filepath.Glob(filepath.Join(sessionDir, "subagents", "agent-*.jsonl"))
	if err != nil {
		return nil
	}
	slices.Sort(matches)
	return matches
}

// buildSubagent reads one agent transcript's sidecar meta + its own tail for tokens/last-tool/mtime.
// Token totals come from this file alone: transcript.LastUsage/usageFromLine skip isSidechain
// records, and every subagent transcript line IS a sidechain, so the per-model aggregate the main
// session reads would silently see zero — transcript.SummarizeFile has no such filter.
func buildSubagent(transcriptPath string, now time.Time) Subagent {
	meta := readSubagentMeta(transcriptPath)
	id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(transcriptPath), "agent-"), ".jsonl")

	var updated time.Time
	if info, err := os.Stat(transcriptPath); err == nil {
		updated = info.ModTime()
	}

	var tok Tokens
	if summary, err := transcript.SummarizeFile(transcriptPath); err == nil {
		t := summary.Totals()
		tok = Tokens{In: t.InputTokens + t.CacheReadTokens + t.CacheCreationTokens, Out: t.OutputTokens}
	}

	return Subagent{
		ID:          id,
		Type:        meta.AgentType,
		Description: meta.Description,
		Status:      subagentStatus(meta.Finished, updated, now),
		Depth:       meta.SpawnDepth,
		Tokens:      tok,
		LastTool:    lastToolName(transcriptPath),
		Updated:     updated,
	}
}

// subagentStatus: finished wins outright; otherwise running within activeWindow of now, else stale.
func subagentStatus(finished bool, updated, now time.Time) string {
	if finished {
		return statusFinished
	}
	if !updated.IsZero() && now.Sub(updated) <= activeWindow {
		return statusRunning
	}
	return statusStale
}

// buildWorkflowRuns reads one session's workflows/wf_*.json manifests (run id, name, result status)
// and folds in the matching subagents/workflows/<runId>/agent-*.jsonl transcripts by workflowPhase,
// via internal/usage's exported helpers — one run id links both halves.
func buildWorkflowRuns(sessionDir string, now time.Time) []WorkflowRun {
	manifests := workflowManifests(sessionDir)
	runDirs := usage.WorkflowRunDirs(sessionDir)
	seen := map[string]bool{}
	var runs []WorkflowRun

	addRun := func(runID string) {
		if seen[runID] {
			return
		}
		seen[runID] = true
		runs = append(runs, buildOneWorkflowRun(sessionDir, runID, manifests[runID], now))
	}
	for id := range manifests {
		addRun(id)
	}
	for _, dir := range runDirs {
		addRun(filepath.Base(dir))
	}
	slices.SortStableFunc(runs, func(a, b WorkflowRun) int { return b.Updated.Compare(a.Updated) })
	return runs
}

// buildOneWorkflowRun folds one run's manifest (possibly absent, for a run still writing) with its
// agent-*.jsonl transcripts grouped by workflowPhase.
func buildOneWorkflowRun(sessionDir, runID string, m *workflowManifest, now time.Time) WorkflowRun {
	run := WorkflowRun{ID: runID, Name: runID}
	if m != nil {
		run.Name = cmpOr(m.name(), runID)
		run.Status = m.Result.Status
		run.Updated = m.timestamp()
	}

	runDir := filepath.Join(sessionDir, "subagents", "workflows", runID)
	files := usage.WorkflowAgentFiles(runDir)
	byPhase := map[string]*WorkflowPhase{}
	var order []string
	for _, f := range files {
		sub := buildSubagent(f, now)
		phaseName := cmpOr(usage.WorkflowPhaseFromMeta(f), "(none)")
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
	return run
}

// cmpOr returns a when non-empty, else b.
func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
