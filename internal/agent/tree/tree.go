// tree.go builds `claude-code agents tree`: every recent session, its subagents and Workflow phases.

package tree

import (
	"cmp"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/transcript"
)

// subagentActiveWindow is how recently a subagent transcript must be written to count as running, else stale.
const subagentActiveWindow = 10 * time.Minute

// sessionActiveWindow is the tighter window for a main session to count as active; past it the session is idle.
const sessionActiveWindow = 2 * time.Minute

// Tokens summarizes a session or subagent: In is input + cache-creation, CacheRead is split out.
type Tokens struct {
	In        int64 `json:"in"`
	Out       int64 `json:"out"`
	CacheRead int64 `json:"cacheRead"`
}

func (t *Tokens) add(o Tokens) {
	t.In += o.In
	t.Out += o.Out
	t.CacheRead += o.CacheRead
}

// Subagent is one subagents/agent-<id>.jsonl + its sibling .meta.json.
type Subagent struct {
	ID          string     `json:"id"`
	Type        string     `json:"type"`
	Description string     `json:"description"`
	Status      string     `json:"status"` // running|finished|error|stale
	Depth       int        `json:"depth"`
	Tokens      Tokens     `json:"tokens"`
	LastTool    string     `json:"lastTool"`
	Updated     time.Time  `json:"updated"`
	Activity    *Activity  `json:"activity,omitempty"`   // newest tool/text signal; nil when none seen yet
	Dispatches  []Dispatch `json:"dispatches,omitempty"` // agent dispatches this relay agent launched via `ask`
}

// Activity is the newest tool_use and assistant text in a subagent transcript tail; At stamps whichever set last.
type Activity struct {
	Tool string    `json:"tool,omitempty"`
	Arg  string    `json:"arg,omitempty"`
	At   time.Time `json:"at"`
	Text string    `json:"text,omitempty"`
}

// Dispatch is one `claude-code ask` a relay subagent launched, read from state/harness/dispatch/<id>.json.
type Dispatch struct {
	ID             string `json:"id"`
	Agent          string `json:"agent"`
	Provider       string `json:"provider,omitempty"`
	Model          string `json:"model,omitempty"`
	Status         string `json:"status"` // running|ok|error, mirroring agent.Record
	Duration       string `json:"duration,omitempty"`
	Fallback       string `json:"fallback,omitempty"`
	FallbackReason string `json:"fallbackReason,omitempty"`
	Matched        string `json:"matched,omitempty"` // "heuristic" when inferred rather than read directly
	Repo           string `json:"repo,omitempty"`    // main checkout root; set on the machine-wide running list

	sessionID string    // dispatch record's session id; matches a duplicate session row
	cwd       string    // dispatch workdir (repo fallback); matches a duplicate session row
	started   time.Time // dispatch start; a matching session must be updated at or after it
	prompt    string    // dispatch prompt; a cwd match must also open with it, so a user's own session survives
}

// WorkflowPhase folds a run's agents under the workflowPhase their meta.json names ("(none)" when absent).
type WorkflowPhase struct {
	Name   string     `json:"name"`
	Agents []Subagent `json:"agents"`
}

// WorkflowRun is one workflows/wf_<id>.json manifest plus its subagents/workflows/<runId>/agent-*.jsonl phases.
type WorkflowRun struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Status  string          `json:"status"` // the manifest's result.status when its result carries one (e.g. "green"), else journal-derived running|finished
	Updated time.Time       `json:"updated"`
	Phases  []WorkflowPhase `json:"phases"`
}

// Session is one projects/<slug>/<session>.jsonl plus everything nested under projects/<slug>/<session>/.
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
	Name      string        `json:"name,omitempty"`      // live registry (sessions/<pid>.json) name
	PID       int           `json:"pid,omitempty"`       // live CLI process id
	Live      bool          `json:"live,omitempty"`      // a live registry entry names this session
	CLIStatus string        `json:"cliStatus,omitempty"` // registry status: idle|busy|waiting
	Mode      string        `json:"mode,omitempty"`      // launch mode: interactive|bg
}

// Tree is the stable `agents tree --format json` schema; live sessions also carry registry fields.
type Tree struct {
	Sessions   []Session  `json:"sessions"`
	Dispatches []Dispatch `json:"dispatches,omitempty"` // unscoped views: live dispatch jobs machine-wide
}

// Options scopes a Build by Days (ignored with Session), one Session id, or Running only; Now defaults to time.Now.
type Options struct {
	Days    int
	Session string
	Running bool
	Now     func() time.Time
}

func (o Options) now() time.Time { return nowOr(o.Now) }

// nowOr returns f() when f is set, else time.Now().
func nowOr(f func() time.Time) time.Time {
	if f != nil {
		return f()
	}
	return time.Now()
}

// Listing is the tree the `agents tree` verb shows: Build, the idle filter unless showAll, and running dispatches.
func Listing(claudeDir string, opts Options, showAll bool) (*Tree, error) {
	tree, err := Build(claudeDir, opts)
	if err != nil {
		return nil, err
	}
	if !showAll {
		tree.Sessions = filterIdleSessions(tree.Sessions)
	}
	if opts.Session == "" { // every view lists them, so --background stays a superset of the default
		tree.Dispatches = RunningDispatches(opts.now())
		tree.Sessions = dropDispatchSessions(tree.Sessions, tree.Dispatches)
	}
	return tree, nil
}

// Build scans claudeDir/projects for every session in scope, newest first.
func Build(claudeDir string, opts Options) (*Tree, error) {
	now := opts.now()
	sessionFiles, err := sessionTranscriptsInScope(claudeDir, opts, now)
	if err != nil {
		return nil, err
	}
	reg := readRegistry(claudeDir)
	if opts.Running && opts.Session == "" {
		have := map[string]bool{}
		for _, f := range sessionFiles {
			have[sessionIDFromPath(f)] = true
		}
		sessionFiles = append(sessionFiles, liveTranscripts(claudeDir, reg, have)...)
	}
	tree := &Tree{}
	for _, f := range sessionFiles {
		s := buildSession(f, now)
		if e, ok := reg[s.ID]; ok {
			s = applyRegistry(s, e)
		}
		if opts.Running && !s.Live && !sessionHasActivity(s) {
			continue
		}
		if opts.Running {
			s = pruneToRunning(s)
		}
		tree.Sessions = append(tree.Sessions, s)
	}
	slices.SortStableFunc(tree.Sessions, func(a, b Session) int { return b.Updated.Compare(a.Updated) })
	return tree, nil
}

// sessionHasActivity reports whether --running keeps s: it is active or any subagent (direct or in a run) runs.
func sessionHasActivity(s Session) bool {
	if s.Status == statusActive {
		return true
	}
	for _, sub := range s.Subagents {
		if sub.Status == StatusRunning {
			return true
		}
	}
	running := false
	for _, run := range s.Workflows {
		forEachRunAgent(run, func(sub Subagent) { running = running || sub.Status == StatusRunning })
	}
	return running
}

// forEachRunAgent calls fn on every agent of every phase of run.
func forEachRunAgent(run WorkflowRun, fn func(Subagent)) {
	for _, phase := range run.Phases {
		for _, sub := range phase.Agents {
			fn(sub)
		}
	}
}

// pruneToRunning returns a copy without finished/error subagents and emptied runs; stale ones stay.
func pruneToRunning(s Session) Session {
	done := func(sub Subagent) bool { return sub.Status == StatusFinished || sub.Status == StatusError }
	s.Subagents = slices.DeleteFunc(slices.Clone(s.Subagents), done)
	var runs []WorkflowRun
	for _, run := range s.Workflows {
		var phases []WorkflowPhase
		for _, ph := range run.Phases {
			ph.Agents = slices.DeleteFunc(slices.Clone(ph.Agents), done)
			if len(ph.Agents) > 0 {
				phases = append(phases, ph)
			}
		}
		run.Phases = phases
		if len(phases) > 0 || run.Status == StatusRunning {
			runs = append(runs, run)
		}
	}
	s.Workflows = runs
	return s
}

const (
	statusActive   = "active"
	statusIdle     = "idle"
	StatusRunning  = "running"
	StatusFinished = "finished"
	StatusError    = "error"
	StatusStale    = "stale"
)

// sessionTranscriptsInScope lists the projects/*/<session>.jsonl files for --session, else those within --days.
func sessionTranscriptsInScope(claudeDir string, opts Options, now time.Time) ([]string, error) {
	if opts.Session != "" {
		matches, ok := plainSessionGlob(claudeDir, opts.Session)
		if !ok {
			return nil, fmt.Errorf("agents tree: --session %q is not a plain session id", opts.Session)
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
	id := sessionIDFromPath(transcriptPath)
	sessionDir := sessionDirFromPath(transcriptPath)
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

	for _, agentFile := range transcript.WorkflowAgentFiles(filepath.Join(sessionDir, "subagents")) {
		sub := buildSubagent(agentFile, now, s.Cwd)
		s.Tokens.add(sub.Tokens)
		s.Subagents = append(s.Subagents, sub)
	}
	slices.SortStableFunc(s.Subagents, func(a, b Subagent) int { return b.Updated.Compare(a.Updated) })

	for _, run := range buildWorkflowRuns(sessionDir, now, s.Cwd) {
		forEachRunAgent(run, func(sub Subagent) { s.Tokens.add(sub.Tokens) })
		s.Workflows = append(s.Workflows, run)
	}
	return s
}

// sessionStatus: active when the transcript was written within sessionActiveWindow of now, else idle.
func sessionStatus(updated, now time.Time) string {
	if !updated.IsZero() && now.Sub(updated) <= sessionActiveWindow {
		return statusActive
	}
	return statusIdle
}

// plainSessionGlob globs projects/*/<id>.jsonl; ok is false when id is not a plain name (path traversal guard).
func plainSessionGlob(claudeDir, id string) (matches []string, ok bool) {
	if id != filepath.Base(id) || id == ".." {
		return nil, false
	}
	matches, _ = filepath.Glob(filepath.Join(claudeDir, "projects", "*", id+".jsonl"))
	return matches, true
}

// buildSubagent reads an agent transcript's sidecar meta, tail and mtime; tokens come from SummarizeFile.
func buildSubagent(transcriptPath string, now time.Time, cwd string) Subagent {
	meta := readSubagentMeta(transcriptPath)
	id := subagentIDFromPath(transcriptPath)

	var updated time.Time
	if info, err := os.Stat(transcriptPath); err == nil {
		updated = info.ModTime()
	}

	var tok Tokens
	if summary, err := transcript.SummarizeFile(transcriptPath); err == nil {
		t := summary.Totals()
		tok = Tokens{In: t.InputTokens + t.CacheCreationTokens, Out: t.OutputTokens, CacheRead: t.CacheReadTokens}
	}

	act := lastActivity(transcriptPath, cwd)
	sub := Subagent{
		ID:          id,
		Type:        meta.AgentType,
		Description: meta.Description,
		Status:      subagentStatus(meta.Finished, updated, now),
		Depth:       meta.SpawnDepth,
		Tokens:      tok,
		LastTool:    act.Tool,
		Updated:     updated,
		Dispatches:  buildDispatches(transcriptPath, cwd, now, act),
	}
	if act.Tool != "" || act.Text != "" {
		a := act
		sub.Activity = &a
	}
	return sub
}

// sessionIDFromPath extracts the session id from <session>.jsonl.
func sessionIDFromPath(transcriptPath string) string {
	return strings.TrimSuffix(filepath.Base(transcriptPath), ".jsonl")
}

// sessionDirFromPath is the <session>/ dir that sits beside <session>.jsonl.
func sessionDirFromPath(transcriptPath string) string {
	return strings.TrimSuffix(transcriptPath, ".jsonl")
}

// subagentIDFromPath extracts the bare id from agent-<id>.jsonl, shared by buildSubagent and journal lookups.
func subagentIDFromPath(transcriptPath string) string {
	return strings.TrimSuffix(strings.TrimPrefix(filepath.Base(transcriptPath), "agent-"), ".jsonl")
}

// subagentStatus: finished wins outright; otherwise running within subagentActiveWindow of now, else stale.
func subagentStatus(finished bool, updated, now time.Time) string {
	if finished {
		return StatusFinished
	}
	if !updated.IsZero() && now.Sub(updated) <= subagentActiveWindow {
		return StatusRunning
	}
	return StatusStale
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

// FilterIdleSessions is filterIdleSessions, exported for callers outside this package.
func FilterIdleSessions(sessions []Session) []Session { return filterIdleSessions(sessions) }

// filterIdleSessions drops idle sessions that never dispatched a subagent or workflow run.
func filterIdleSessions(sessions []Session) []Session {
	var kept []Session
	for _, s := range sessions {
		if !s.Live && s.Status == statusIdle && len(s.Subagents) == 0 && len(s.Workflows) == 0 {
			continue
		}
		kept = append(kept, s)
	}
	return kept
}
