// Package agenttree builds `claude-code agents tree`: every recent session across every project,
// its subagents (and the Workflow runs/phases inside it), read straight off ~/.claude/projects.
package agenttree

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/transcript"
	"github.com/edotau/claude-code/internal/usage"
)

// subagentActiveWindow is how recently a subagent transcript must have been written to count as
// "running"; past it but unfinished is "stale". Subagents go quiet between tool calls for longer
// than a few seconds, so this window is generous.
const subagentActiveWindow = 10 * time.Minute

// sessionActiveWindow is the much tighter window for a main session's own "active" status: Claude
// Code appends to that transcript continuously while a turn is in flight, so anything but "just
// happened" means the session has gone idle, regardless of how long a dispatched subagent keeps running.
const sessionActiveWindow = 2 * time.Minute

// Tokens is a compact token summary, used for both a session and a subagent. In is input +
// cache-creation (the tokens actually billed fresh); CacheRead is split out separately since a
// cache hit is nearly free and folding it into In makes the number meaningless to a reader.
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
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	Description string    `json:"description"`
	Status      string    `json:"status"` // running|finished|error|stale
	Depth       int       `json:"depth"`
	Tokens      Tokens    `json:"tokens"`
	LastTool    string    `json:"lastTool"`
	Updated     time.Time `json:"updated"`
	Activity    *Activity `json:"activity,omitempty"` // newest tool/text signal; nil when none seen yet
	Legs        []Leg     `json:"legs,omitempty"`     // vendor dispatches this relay agent launched via `ask`
}

// Activity is the newest tool_use and newest assistant text seen in a subagent transcript's tail —
// the "what is it doing right now" signal. At is the timestamp of whichever record set it last;
// Subagent carries Activity behind a pointer (omitempty there), so At needs no zero-value tag of its
// own — "activity,omitempty" on encoding/json's struct-typed omitempty is a no-op (never omits).
type Activity struct {
	Tool string    `json:"tool,omitempty"`
	Arg  string    `json:"arg,omitempty"`
	At   time.Time `json:"at"`
	Text string    `json:"text,omitempty"`
}

// Leg is one vendor dispatch (`claude-code ask`) a relay subagent launched, read from
// state/harness/dispatch/<id>.json via internal/agent.
type Leg struct {
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
	Status  string          `json:"status"` // the manifest's result.status when its result carries one (e.g. "green"), else journal-derived running|finished
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
	Sessions   []Session `json:"sessions"`
	Dispatches []Leg     `json:"dispatches,omitempty"` // --running only: live dispatch jobs machine-wide
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
		if opts.Running {
			s = pruneToRunning(s)
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

// pruneToRunning drops finished/error subagents (with their legs) and workflow runs left with none;
// stale ones stay, since an unfinished agent past the window may be stuck. Returns a copy.
func pruneToRunning(s Session) Session {
	done := func(sub Subagent) bool { return sub.Status == statusFinished || sub.Status == statusError }
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
		if len(phases) > 0 || run.Status == statusRunning {
			runs = append(runs, run)
		}
	}
	s.Workflows = runs
	return s
}

const (
	statusActive   = "active"
	statusIdle     = "idle"
	statusRunning  = "running"
	statusFinished = "finished"
	statusError    = "error"
	statusStale    = "stale"
)

// sessionTranscriptsInScope lists the projects/*/<session>.jsonl files in scope: every project dir
// for --session, else every one modified within --days.
func sessionTranscriptsInScope(claudeDir string, opts Options, now time.Time) ([]string, error) {
	if opts.Session != "" {
		if opts.Session != filepath.Base(opts.Session) || opts.Session == ".." {
			return nil, fmt.Errorf("agents tree: --session %q is not a plain session id", opts.Session)
		}
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
		sub := buildSubagent(agentFile, now, s.Cwd)
		s.Tokens.add(sub.Tokens)
		s.Subagents = append(s.Subagents, sub)
	}
	slices.SortStableFunc(s.Subagents, func(a, b Subagent) int { return b.Updated.Compare(a.Updated) })

	for _, run := range buildWorkflowRuns(sessionDir, now, s.Cwd) {
		for _, phase := range run.Phases {
			for _, sub := range phase.Agents {
				s.Tokens.add(sub.Tokens)
			}
		}
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

// buildSubagent reads one agent transcript's sidecar meta + its own tail for tokens/activity/mtime.
// Token totals come from this file alone: transcript.LastUsage/usageFromLine skip isSidechain
// records, and every subagent transcript line IS a sidechain, so the per-model aggregate the main
// session reads would silently see zero — transcript.SummarizeFile has no such filter. cwd is the
// owning session's cwd, used to strip the repo root from Activity.Arg and to match a vendor leg's
// own WorkDir.
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
		Legs:        buildLegs(transcriptPath, cwd, now, act),
	}
	if act.Tool != "" || act.Text != "" {
		a := act
		sub.Activity = &a
	}
	return sub
}

// subagentIDFromPath extracts the bare id from agent-<id>.jsonl, shared by buildSubagent and the
// journal/vendor-leg lookups that key on the same id.
func subagentIDFromPath(transcriptPath string) string {
	return strings.TrimSuffix(strings.TrimPrefix(filepath.Base(transcriptPath), "agent-"), ".jsonl")
}

// subagentStatus: finished wins outright; otherwise running within subagentActiveWindow of now, else stale.
func subagentStatus(finished bool, updated, now time.Time) string {
	if finished {
		return statusFinished
	}
	if !updated.IsZero() && now.Sub(updated) <= subagentActiveWindow {
		return statusRunning
	}
	return statusStale
}

// buildWorkflowRuns reads one session's workflows/wf_*.json manifests (run id, name, result status)
// and folds in the matching subagents/workflows/<runId>/agent-*.jsonl transcripts by workflowPhase,
// via internal/usage's exported helpers — one run id links both halves.
func buildWorkflowRuns(sessionDir string, now time.Time, cwd string) []WorkflowRun {
	manifests := workflowManifests(sessionDir)
	runDirs := usage.WorkflowRunDirs(sessionDir)
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

// buildOneWorkflowRun folds one run's manifest (possibly absent, for a run still writing) with its
// agent-*.jsonl transcripts grouped by workflowPhase; journal.jsonl in the run dir overrides each
// agent's status (finished/error once it has a result line) and backfills a missing description.
func buildOneWorkflowRun(sessionDir, runID string, m *workflowManifest, now time.Time, cwd string) WorkflowRun {
	run := WorkflowRun{ID: runID, Name: runID}
	if m != nil {
		run.Name = cmp.Or(m.name(), runID)
		run.Updated = m.timestamp()
	}

	runDir := filepath.Join(sessionDir, "subagents", "workflows", runID)
	journal := readJournal(runDir)
	files := usage.WorkflowAgentFiles(runDir)
	byPhase := map[string]*WorkflowPhase{}
	var order []string
	for _, f := range files {
		sub := buildSubagent(f, now, cwd)
		sub = applyJournalEntry(sub, journal[sub.ID])
		phaseName := cmp.Or(usage.WorkflowPhaseFromMeta(f), "(none)")
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

// applyJournalEntry overrides a workflow subagent's status/description from its journal entry: a
// result line wins outright (finished, or error when that result was JSON null); started-only
// leaves today's mtime-driven running/stale read in place. A zero-value entry (not found) is a no-op.
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

// workflowRunStatus prefers the manifest's own result status (e.g. a green-keeper-loop's "green"/"red" when
// its script returned an object carrying one); otherwise it's journal-derived: finished once every
// started agent has a result and nothing has touched the run dir for subagentActiveWindow, else running.
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

// FilterIdleSessions is filterIdleSessions, exported for callers outside this package (e.g. the
// `web` dashboard) that want the CLI's default idle filtering applied to a Build result.
func FilterIdleSessions(sessions []Session) []Session { return filterIdleSessions(sessions) }

// filterIdleSessions drops an idle session with no subagents and no workflow runs — a session
// opened and never dispatched anything is noise in the view, not state worth paging through.
// Callers that want every session regardless (--all) skip calling this.
func filterIdleSessions(sessions []Session) []Session {
	var kept []Session
	for _, s := range sessions {
		if s.Status == statusIdle && len(s.Subagents) == 0 && len(s.Workflows) == 0 {
			continue
		}
		kept = append(kept, s)
	}
	return kept
}
