// tree.go builds `claude-code agents tree`: every recent session, its subagents and Workflow phases.

package tree

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/proc"
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

// Dispatch is one `claude-code agents ask` a relay subagent launched, read from state/harness/dispatch/<id>.json.
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
	if opts.Session == "" { // live sessions stay listed past --days, under --background too
		sessionFiles = appendLiveTranscripts(claudeDir, reg, sessionFiles)
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

func appendLiveTranscripts(claudeDir string, reg map[string]regEntry, sessionFiles []string) []string {
	have := map[string]bool{}
	for _, f := range sessionFiles {
		have[sessionIDFromPath(f)] = true
	}
	return append(sessionFiles, liveTranscripts(claudeDir, reg, have)...)
}

// sessionHasActivity reports whether --running keeps s: it is active or any subagent (direct or in a run) is open.
func sessionHasActivity(s Session) bool {
	if s.Status == statusActive {
		return true
	}
	if slices.ContainsFunc(s.Subagents, unfinished) {
		return true
	}
	running := false
	for _, run := range s.Workflows {
		forEachRunAgent(run, func(sub Subagent) { running = running || unfinished(sub) })
	}
	return running
}

// unfinished is true for a subagent pruneToRunning keeps: running or stale, not finished or error.
func unfinished(sub Subagent) bool {
	return sub.Status != StatusFinished && sub.Status != StatusError
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
		return plainSessionGlob(claudeDir, opts.Session)
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

// plainSessionGlob globs projects/*/<id>.jsonl; an id with a separator or glob metacharacter is rejected, never globbed.
func plainSessionGlob(claudeDir, id string) ([]string, error) {
	if id != filepath.Base(id) || id == ".." || strings.ContainsAny(id, `*?[]\`) {
		return nil, fmt.Errorf("agents tree: --session %q is not a plain session id", id)
	}
	return filepath.Glob(filepath.Join(claudeDir, "projects", "*", id+".jsonl"))
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
		if have[id] {
			continue
		}
		matches, err := plainSessionGlob(claudeDir, id)
		if err != nil {
			continue // a registry name that is not a plain session id has no transcript
		}
		files = append(files, matches...)
	}
	return files
}
