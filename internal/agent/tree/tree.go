// tree.go builds `claude-code agents tree`: every recent session, its subagents and Workflow phases.

package tree

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/parallel"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/transcript"
	"github.com/edotau/claude-code/internal/worktree"
)

// subagentActiveWindow is how recently a subagent transcript must be written to count as running, else stale.
const subagentActiveWindow = 10 * time.Minute

// sessionActiveWindow is the tighter window for a main session to count as active; past it the session is idle.
const sessionActiveWindow = 2 * time.Minute

type subtreeTotal struct {
	sum             int64
	ready, children bool
}

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
	total       subtreeTotal
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
	Files       []string   `json:"files,omitempty"`      // --files: still-dirty files this agent wrote, repo-relative
	Phase       string     `json:"-"`                    // workflowPhase from the sidecar meta, for run grouping
	Model       string     `json:"-"`                    // transcript's newest message.model; the provider shows in its prefix
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
	total          subtreeTotal
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
	Route          string `json:"route,omitempty"`   // the overlay confinement it was launched on; "" is no overlay confinement
	Task           string `json:"task,omitempty"`    // first line of the brief's task, past any Owned/Mode/Verify header

	Files []string `json:"files,omitempty"` // --files: files the run wrote that are still dirty, repo-relative
	Note  string   `json:"note,omitempty"`  // --files: the run's latest undelivered note to its dispatcher

	Parent string      `json:"parent,omitempty"` // id of the dispatch that launched this one
	Depth  int         `json:"depth,omitempty"`  // 0 for a dispatch launched outside any other
	Usage  *wire.Usage `json:"usage,omitempty"`  // the record's own token counts, never a subtree sum

	LastSignal   time.Time `json:"lastSignal,omitzero"`    // running only: newest of the event file, log or start time
	SignalSource string    `json:"signalSource,omitempty"` // which of events|log|record LastSignal came from
	Quiet        bool      `json:"quiet,omitempty"`        // LastSignal older than quietAfter
	Live         string    `json:"live,omitempty"`         // running only: the progress fragment (no agent, model, duration), also the text row's signal

	sessionID string    // dispatch record's session id; matches a duplicate session row
	cwd       string    // dispatch workdir (repo fallback); matches a duplicate session row
	started   time.Time // dispatch start; a matching session must be updated at or after it
	prompt    string    // dispatch prompt; a cwd match must also open with it, so a user's own session survives

	carry    bool      // the outer record of a carried dispatch: its row folds in the child doing the work
	takeover bool      // a Pi takeover that finished a cut-off carried child
	launch   string    // the profile, else provider, the request was launched on; "" when it named none
	queued   string    // the serve pool the job waits on for admission; "" once admitted
	queuedAt time.Time // when that admission wait began
}

// WorkflowPhase folds a run's agents under the workflowPhase their meta.json names ("(none)" when absent).
type WorkflowPhase struct {
	total  subtreeTotal
	done   int        // agents with a result in, counted before the active-only filter
	count  int        // all agents, counted before the active-only filter
	Name   string     `json:"name"`
	Agents []Subagent `json:"agents"`
}

// WorkflowRun is one workflows/wf_<id>.json manifest plus its subagents/workflows/<runId>/agent-*.jsonl phases.
type WorkflowRun struct {
	total   subtreeTotal
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Status  string          `json:"status"` // the manifest's result.status when its result carries one (e.g. "green"), else journal-derived running|finished
	Updated time.Time       `json:"updated"`
	Phases  []WorkflowPhase `json:"phases"`
}

// Session is one projects/<slug>/<session>.jsonl plus everything nested under projects/<slug>/<session>/.
type Session struct {
	total     subtreeTotal
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
	Notes     []string      `json:"notes,omitempty"`     // --files: undelivered notes from this session's subagents
}

// Tree is the stable `agents tree --format json` schema; live sessions also carry registry fields.
type Tree struct {
	Sessions   []Session  `json:"sessions"`
	Dispatches []Dispatch `json:"dispatches,omitempty"` // unscoped views: live dispatch jobs machine-wide
	Free       []string   `json:"free,omitempty"`       // --files: dirty files in this repo that no live leaf holds

	detail bool // --detail: each dispatch row is followed by its id and task
}

// Options scopes a build by Days (ignored with Session) or one Session id; Now defaults to time.Now.
type Options struct {
	Days    int
	Session string
	Now     func() time.Time
	Files   bool       // add the files each leaf holds, its latest note, and the free files
	scans   *scanCache // kept across watch ticks; nil loads and saves one per build
}

// now is o.Now() when set, else time.Now().
func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// listing is the tree the `agents tree` verb shows: build, the idle filter unless showAll, and running dispatches.
func listing(claudeDir string, opts Options, showAll bool) (*Tree, error) {
	tree, err := build(claudeDir, opts)
	if err != nil {
		return nil, err
	}
	if !showAll {
		tree.Sessions = filterIdleSessions(tree.Sessions)
		for i, s := range tree.Sessions {
			tree.Sessions[i] = activeSession(s)
		}
	}
	if opts.Session == "" { // every view lists them, so --detail stays a superset of the default
		tree.Dispatches = runningDispatches(opts.now())
		cacheDispatchTotals(tree.Dispatches)
		tree.Sessions = dropDispatchSessions(tree.Sessions, tree.Dispatches)
	}
	if opts.Files {
		attachHoldings(tree, cwdTop(), opts.now())
	}
	return tree, nil
}

// build scans claudeDir/projects for every session in scope, newest first.
func build(claudeDir string, opts Options) (*Tree, error) {
	now := opts.now()
	sessionFiles, all, err := sessionTranscriptsInScope(claudeDir, opts, now)
	if err != nil {
		return nil, err
	}
	reg := readRegistry(claudeDir)
	if opts.Session == "" { // live sessions stay listed past --days, under --detail too
		sessionFiles = appendLiveTranscripts(all, reg, sessionFiles)
	}
	scans := opts.scans
	if scans == nil {
		scans = loadScanCache()
	} else {
		scans.next()
	}
	defer saveScanCache(scans)
	tree := &Tree{}
	for _, f := range sessionFiles {
		s := buildSession(f, now, scans)
		if e, ok := reg[s.ID]; ok {
			s = applyRegistry(s, e)
		}
		tree.Sessions = append(tree.Sessions, s)
	}
	slices.SortStableFunc(tree.Sessions, func(a, b Session) int { return b.Updated.Compare(a.Updated) })
	return tree, nil
}

func appendLiveTranscripts(all []transcriptFile, reg map[string]regEntry, sessionFiles []string) []string {
	have := map[string]bool{}
	for _, f := range sessionFiles {
		have[sessionIDFromPath(f)] = true
	}
	return append(sessionFiles, liveTranscripts(all, reg, have)...)
}

// finished is true for a subagent with a result in: finished or error.
func finished(sub Subagent) bool {
	return sub.Status == statusFinished || sub.Status == statusError
}

// forEachRunAgent calls fn on every agent of every phase of run.
func forEachRunAgent(run WorkflowRun, fn func(Subagent)) {
	for _, phase := range run.Phases {
		for _, sub := range phase.Agents {
			fn(sub)
		}
	}
}

const (
	statusActive   = "active"
	statusIdle     = "idle"
	statusRunning  = "running"
	statusFinished = "finished"
	statusError    = "error"
	statusStale    = "stale"
)

// transcriptFile is one projects/*/*.jsonl match; ok is false when its Stat failed.
type transcriptFile struct {
	path  string
	mtime time.Time
	ok    bool
}

// files honours --session, else --days; all is every projects/*/*.jsonl in glob order, nil for --session.
func sessionTranscriptsInScope(claudeDir string, opts Options, now time.Time) (files []string, all []transcriptFile, err error) {
	if opts.Session != "" {
		files, err = plainSessionGlob(claudeDir, opts.Session)
		return files, nil, err
	}
	days := opts.Days
	if days <= 0 {
		days = 1
	}
	all = projectTranscripts(claudeDir)
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	for _, f := range all {
		if f.ok && !f.mtime.Before(cutoff) {
			files = append(files, f.path)
		}
	}
	slices.Sort(files)
	return files, all, nil
}

// projectTranscripts is filepath.Glob(projects/*/*.jsonl) with each match stat-ed, one project dir per worker.
func projectTranscripts(claudeDir string) []transcriptFile {
	dirs := sortedNames(filepath.Join(claudeDir, "projects"))
	perDir, _ := parallel.Map(context.Background(), dirs, 8, func(_ context.Context, d string) ([]transcriptFile, error) {
		dir := filepath.Join(claudeDir, "projects", d)
		var out []transcriptFile
		for _, n := range sortedNames(dir) {
			if ok, _ := filepath.Match("*.jsonl", n); !ok {
				continue
			}
			f := transcriptFile{path: filepath.Join(dir, n)}
			if info, err := os.Stat(f.path); err == nil {
				f.mtime, f.ok = info.ModTime(), true
			}
			out = append(out, f)
		}
		return out, nil
	})
	return slices.Concat(perDir...)
}

// sortedNames is dir's entry names sorted, as filepath.Glob reads them; an unreadable or non-dir path has none.
func sortedNames(dir string) []string {
	d, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer d.Close() // read-only handle: a close error loses nothing
	names, _ := d.Readdirnames(-1)
	slices.Sort(names)
	return names
}

// buildSession reads one session's main transcript, its subagents dir and its workflow runs.
func buildSession(transcriptPath string, now time.Time, scans *scanCache) Session {
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
		sub := buildSubagent(agentFile, now, s.Cwd, scans)
		s.Tokens.add(sub.Tokens)
		s.Subagents = append(s.Subagents, sub)
	}
	slices.SortStableFunc(s.Subagents, func(a, b Subagent) int { return b.Updated.Compare(a.Updated) })

	s.Workflows = buildWorkflowRuns(sessionDir, now, s.Cwd, scans)
	for _, run := range s.Workflows {
		forEachRunAgent(run, func(sub Subagent) { s.Tokens.add(sub.Tokens) })
	}
	return cacheSessionTotals(s)
}

// sessionStatus: active when the transcript was written within sessionActiveWindow of now, else idle.
func sessionStatus(updated, now time.Time) string {
	if !updated.IsZero() && now.Sub(updated) <= sessionActiveWindow {
		return statusActive
	}
	return statusIdle
}

// plainSessionGlob globs projects/*/<id>.jsonl; an id with a separator or glob metacharacter is rejected.
func plainSessionGlob(claudeDir, id string) ([]string, error) {
	if !plainSessionID(id) {
		return nil, fmt.Errorf("agents tree: --session %q is not a plain session id", id)
	}
	return filepath.Glob(filepath.Join(claudeDir, "projects", "*", id+".jsonl"))
}

// plainSessionID is an id with no separator or glob metacharacter.
func plainSessionID(id string) bool {
	return id == filepath.Base(id) && id != ".." && !strings.ContainsAny(id, `*?[]\`)
}

// buildSubagent reads sidecar meta, tail and mtime; tokens and historical dispatch IDs share one full scan.
func buildSubagent(transcriptPath string, now time.Time, cwd string, scans *scanCache) Subagent {
	id := subagentIDFromPath(transcriptPath)

	var updated time.Time
	if info, err := os.Stat(transcriptPath); err == nil {
		updated = info.ModTime()
	}

	e := scans.scan(transcriptPath)
	meta, phase := scans.meta(e, transcriptPath)
	act := scans.activity(e, transcriptPath, cwd)
	sub := Subagent{
		ID:          id,
		Type:        meta.AgentType,
		Description: meta.Description,
		Status:      subagentStatus(meta.Finished, updated, now),
		Depth:       meta.SpawnDepth,
		Tokens:      e.totals(),
		Phase:       phase,
		Model:       e.Model,
		LastTool:    act.Tool,
		Updated:     updated,
		Dispatches:  buildDispatches(transcriptPath, cwd, now, act, scans.records(e.IDs)),
	}
	if act.Tool != "" || act.Text != "" {
		sub.Activity = &act
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
		return statusFinished
	}
	if !updated.IsZero() && now.Sub(updated) <= subagentActiveWindow {
		return statusRunning
	}
	return statusStale
}

// filterIdleSessions retains ancestors when their descendants are still running.
func filterIdleSessions(sessions []Session) []Session {
	var kept []Session
	for _, s := range sessions {
		if !s.isActive() {
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

// liveTranscripts picks projects/*/<id>.jsonl out of all for each registry entry not already in have.
func liveTranscripts(all []transcriptFile, reg map[string]regEntry, have map[string]bool) []string {
	byName := map[string][]string{}
	for _, f := range all {
		n := filepath.Base(f.path)
		byName[n] = append(byName[n], f.path)
	}
	var files []string
	for id := range reg {
		if have[id] || !plainSessionID(id) {
			continue // a registry name that is not a plain session id has no transcript
		}
		files = append(files, byName[id+".jsonl"]...)
	}
	return files
}

// activeSession hides completed rows without changing full-subtree totals.
func activeSession(s Session) Session {
	s.Subagents = activeSubagents(s.Subagents)
	s.Workflows = slices.Clone(s.Workflows)
	for i, run := range s.Workflows {
		run.Phases = slices.Clone(run.Phases)
		for j, ph := range run.Phases {
			ph.Agents = activeSubagents(ph.Agents)
			run.Phases[j] = ph
		}
		run.Phases = slices.DeleteFunc(run.Phases, func(ph WorkflowPhase) bool { return len(ph.Agents) == 0 })
		s.Workflows[i] = run
	}
	s.Workflows = slices.DeleteFunc(s.Workflows, func(run WorkflowRun) bool {
		return run.Status != statusRunning && len(run.Phases) == 0
	})
	return s
}

// activeSubagents retains running descendants and the ancestors needed to place them.
func activeSubagents(subs []Subagent) []Subagent {
	out := make([]Subagent, 0, len(subs))
	for _, sub := range subs {
		if !sub.running() {
			continue
		}
		if len(sub.Dispatches) > 0 {
			nodes := nestDispatches(sub.Dispatches)
			keep := make(map[string]bool, len(nodes))
			for _, n := range slices.Backward(nodes) {
				if n.Status == statusRunning || keep[n.ID] {
					keep[n.ID], keep[n.Parent] = true, true
				}
			}
			sub.Dispatches = slices.DeleteFunc(slices.Clone(sub.Dispatches), func(d Dispatch) bool { return !keep[d.ID] })
		}
		out = append(out, sub)
	}
	return out
}

// cacheSessionTotals freezes full-subtree usage before either visibility filter runs.
func cacheSessionTotals(s Session) Session {
	s.total = subtreeTotal{sum: s.Tokens.In + s.Tokens.Out + s.Tokens.CacheRead,
		ready: true, children: len(s.Subagents)+len(s.Workflows) > 0}
	for i := range s.Subagents {
		sub := cacheSubagentTotal(s.Subagents[i])
		s.Subagents[i] = sub
		s.total.sum += sub.total.sum - sub.Tokens.In - sub.Tokens.Out - sub.Tokens.CacheRead
	}
	for i, run := range s.Workflows {
		run = cacheWorkflowTotal(run)
		s.Workflows[i] = run
		forEachRunAgent(run, func(sub Subagent) {
			s.total.sum += sub.total.sum - sub.Tokens.In - sub.Tokens.Out - sub.Tokens.CacheRead
		})
	}
	return s
}

func cacheSubagentTotal(sub Subagent) Subagent {
	cacheDispatchTotals(sub.Dispatches)
	sub.total = subtreeTotal{sum: sub.Tokens.In + sub.Tokens.Out + sub.Tokens.CacheRead,
		ready: true, children: len(sub.Dispatches) > 0}
	for _, d := range sub.Dispatches {
		sub.total.sum += d.tokens()
	}
	return sub
}

func cacheWorkflowTotal(run WorkflowRun) WorkflowRun {
	run.total = subtreeTotal{ready: true}
	for i, phase := range run.Phases {
		phase.total = subtreeTotal{ready: true}
		phase.done, phase.count = phaseDone(phase.Agents)
		for j, sub := range phase.Agents {
			sub = cacheSubagentTotal(sub)
			phase.Agents[j] = sub
			phase.total.sum += sub.total.sum
		}
		run.Phases[i] = phase
		run.total.sum += phase.total.sum
	}
	return run
}

func cacheDispatchTotals(ds []Dispatch) {
	if len(ds) == 1 {
		ds[0].total = subtreeTotal{sum: ds[0].tokens(), ready: true}
		return
	}
	for _, n := range nestDispatches(ds) {
		ds[n.index].total = subtreeTotal{sum: n.sum, ready: true, children: n.children}
	}
}

// heldShown and freeShown are how many files a leaf row and the free list show before "(+N)".
const (
	heldShown = 5
	freeShown = 20
)

// attachHoldings fills Files, Note, Notes and Free from claims, notices and records; a failed source warns on stderr.
func attachHoldings(t *Tree, top string, now time.Time) {
	h, err := worktree.ReadHoldings(top, now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agents tree: files:", err)
	}
	t.Free = h.Free
	for i := range t.Sessions {
		s := &t.Sessions[i]
		for j := range s.Subagents {
			attachSubagent(&s.Subagents[j], s.ID, h)
		}
		for _, run := range s.Workflows {
			for _, ph := range run.Phases {
				for j := range ph.Agents {
					attachSubagent(&ph.Agents[j], s.ID, h)
				}
			}
		}
		s.Notes = subagentNotes(s.ID)
	}
	for i := range t.Dispatches {
		attachDispatch(&t.Dispatches[i], h)
	}
}

// cwdTop is the repo the verb runs in; "" outside one.
func cwdTop() string {
	cwd, _ := os.Getwd()
	top, _ := paths.CheckoutOf(cwd)
	return top
}

func attachSubagent(sub *Subagent, session string, h worktree.Holdings) {
	sub.Files = h.Subagent(session, sub.ID)
	for i := range sub.Dispatches {
		attachDispatch(&sub.Dispatches[i], h)
	}
}

// attachDispatch sets the files its record says it wrote and its latest note, read from its owner's inbox.
func attachDispatch(d *Dispatch, h worktree.Holdings) {
	rec, err := agent.LoadRecord(d.ID)
	if err != nil {
		return
	}
	if rec.Wrote != nil {
		d.Files = h.Keep(*rec.Wrote)
	}
	from := worktree.DispatchNoteFrom(rec.ID)
	for _, n := range worktree.PeekNotices(worktree.OwnerSession(rec.Owner), worktree.LeafNoteKind) {
		if n.From == from {
			d.Note = worktree.NoteText(n)
		}
	}
}

// subagentNotes are session's undelivered notes from Agent-tool subagents, which carry no id of their own.
func subagentNotes(session string) []string {
	var out []string
	for _, n := range worktree.PeekNotices(session, worktree.LeafNoteKind) {
		if n.From == worktree.SubagentNoteFrom {
			out = append(out, worktree.NoteText(n))
		}
	}
	return out
}

// addHeld adds the files and note lines under a leaf row; nothing when it holds none.
func addHeld(l *layout, indent int, files []string, note string) {
	if len(files) > 0 {
		shown := files[:min(len(files), heldShown)]
		more := ""
		if len(files) > heldShown {
			more = fmt.Sprintf(" (+%d)", len(files)-heldShown)
		}
		l.free(indent, "files: "+strings.Join(shown, " ")+more)
	}
	if note != "" {
		l.free(indent, "note: "+textutil.OneLine(note))
	}
}

// addFree adds the dirty files no live leaf holds, after the sessions.
func addFree(l *layout, free []string) {
	if len(free) == 0 {
		return
	}
	l.sep()
	l.free(0, fmt.Sprintf("free: %d dirty, held by no live leaf", len(free)))
	for _, f := range free[:min(len(free), freeShown)] {
		l.free(2, f)
	}
	if len(free) > freeShown {
		l.free(2, fmt.Sprintf("(+%d more)", len(free)-freeShown))
	}
}
