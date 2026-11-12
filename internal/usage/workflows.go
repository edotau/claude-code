package usage

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/transcript"
	"github.com/edotau/claude-code/internal/ui/table"
)

// workflowPhaseRow is one phase's agents folded together inside a single run.
type workflowPhaseRow struct {
	phase  string
	agents int
	turns  int
	tokens transcript.ModelUsage
	first  time.Time
	last   time.Time
	models map[string]*modelStats // short model name -> folded usage, for the MODEL column + rollup
}

// modelStats is one short model name's folded usage across however many agent files used it.
type modelStats struct {
	model  string
	agents int
	turns  int
	tokens transcript.ModelUsage
}

// workflowRun is one workflows/<runId> dir: its derived name, its phase rows and its own span.
type workflowRun struct {
	id     string
	name   string
	phases []*workflowPhaseRow
	total  workflowPhaseRow
	mtime  time.Time
}

// runUsageWorkflows prints one block per workflow run, newest first, honouring the same --session /
// --days scoping as the per-model table.
func runUsageWorkflows(claudeDir string, out, errw io.Writer, session string, days int) int {
	runDirs, err := workflowRunDirsForScope(claudeDir, session, days)
	if err != nil {
		fmt.Fprintln(errw, "usage:", err)
		return 1
	}
	cutoff := workflowCutoff(session, days)

	var runs []*workflowRun
	scanned := 0
	for _, dir := range runDirs {
		run, files := loadWorkflowRun(dir, errw, cutoff)
		if run == nil {
			continue
		}
		runs = append(runs, run)
		scanned += files
	}
	if len(runs) == 0 {
		fmt.Fprintln(errw, "usage: no matching workflow runs found")
		return 0
	}

	// Newest first: run ids are random, so recency comes from the last transcript timestamp, with the
	// run dir's mtime as the fallback when no record carried a parseable one.
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].recency().After(runs[j].recency()) })
	w := &textutil.ErrWriter{W: out}
	for i, run := range runs {
		if i > 0 {
			fmt.Fprintln(w)
		}
		printWorkflowRun(w, run)
	}
	printAllRunsByModel(w, runs)
	fmt.Fprintf(w, "\nworkflow runs: %d   agent transcripts scanned: %d\n", len(runs), scanned)
	if w.Err != nil {
		fmt.Fprintln(errw, "usage: write output:", w.Err)
		return 1
	}
	return 0
}

// workflowCutoff is the --days window's start for workflow runs; a zero time (no window) under --session.
func workflowCutoff(session string, days int) time.Time {
	if session != "" {
		return time.Time{}
	}
	return windowCutoff(days)
}

// recency is the run's sort key for newest-first ordering.
func (r *workflowRun) recency() time.Time {
	if !r.total.last.IsZero() {
		return r.total.last
	}
	return r.mtime
}

// workflowRunDirsForScope resolves the run dirs in scope: every project dir for --session, else every
// session dir whose mtime falls within --days (the same gate the per-model table uses).
func workflowRunDirsForScope(claudeDir, session string, days int) ([]string, error) {
	if session != "" {
		return transcript.WorkflowRunDirs(filepath.Join(claudeDir, "projects", "*", session)), nil
	}
	sessionDirs, err := usageSessionDirsWithinDays(claudeDir, days)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, dir := range sessionDirs {
		dirs = append(dirs, transcript.WorkflowRunDirs(dir)...)
	}
	return dirs, nil
}

// loadWorkflowRun folds the in-window responses of every agent transcript in one run dir into phase rows,
// returning the run and the number of transcripts that contributed (nil when none did).
func loadWorkflowRun(runDir string, errw io.Writer, cutoff time.Time) (*workflowRun, int) {
	files := transcript.WorkflowAgentFiles(runDir)
	if len(files) == 0 {
		return nil, 0
	}
	run := &workflowRun{id: filepath.Base(runDir)}
	run.name = workflowNameForRun(runDir, run.id)
	run.total.phase = "TOTAL"
	run.total.models = map[string]*modelStats{}
	if info, err := os.Stat(runDir); err == nil {
		run.mtime = info.ModTime()
	}

	byPhase := map[string]*workflowPhaseRow{}
	scanned := 0
	for _, f := range files {
		resps, err := transcript.ReadResponses(f)
		if err != nil {
			fmt.Fprintf(errw, "usage: warning: skipping %s: %v\n", f, err)
			continue
		}
		kept := inWindowResponses(resps, cutoff)
		if len(kept) == 0 {
			continue
		}
		summary := transcript.SummarizeResponses(kept)
		scanned++
		phase := transcript.WorkflowPhaseFromMeta(f, errw)
		if phase == "" {
			phase = "(none)"
		}
		row := byPhase[phase]
		if row == nil {
			row = &workflowPhaseRow{phase: phase, models: map[string]*modelStats{}}
			byPhase[phase] = row
			run.phases = append(run.phases, row)
		}
		addAgentToRow(row, summary)
		addAgentToRow(&run.total, summary)
	}
	if scanned == 0 {
		return nil, 0
	}
	// Chronological, so the block reads as the run progressed; a phase with no timestamps sorts last.
	sort.SliceStable(run.phases, func(i, j int) bool { return phaseBefore(run.phases[i], run.phases[j]) })
	return run, scanned
}

// inWindowResponses keeps the responses stamped at or after cutoff; an unstamped response is kept, as in foldResponses.
func inWindowResponses(resps []transcript.Response, cutoff time.Time) []transcript.Response {
	kept := make([]transcript.Response, 0, len(resps))
	for _, r := range resps {
		if r.At.IsZero() || !r.At.Before(cutoff) {
			kept = append(kept, r)
		}
	}
	return kept
}

// addAgentToRow folds one agent transcript's summary into a phase (or run-total) row.
func addAgentToRow(row *workflowPhaseRow, s *transcript.FileSummary) {
	t := s.Totals()
	row.agents++
	row.turns += s.Turns
	row.tokens.Add(t)
	if !s.First.IsZero() && (row.first.IsZero() || s.First.Before(row.first)) {
		row.first = s.First
	}
	if s.Last.After(row.last) {
		row.last = s.Last
	}

	// Per-model fold, keyed by short name: an agent that used N short names counts once in each.
	seen := map[string]bool{}
	for rawModel, u := range s.Models {
		short := shortModelName(rawModel)
		if short == "" {
			continue
		}
		ms := row.models[short]
		if ms == nil {
			ms = &modelStats{model: short}
			row.models[short] = ms
		}
		if !seen[short] {
			ms.agents++
			seen[short] = true
		}
		ms.turns += s.ModelTurns[rawModel]
		ms.tokens.Add(*u)
	}
}

// modelDateSuffix strips a trailing -YYYYMMDD date (e.g. haiku-4-5-20251001 -> haiku-4-5); a numeric
// minor version like -4-8 or -5-5 has fewer than 8 digits and is left alone.
var modelDateSuffix = regexp.MustCompile(`-\d{8}$`)

// modelContextSuffix strips a trailing context-window tag such as "[1m]".
var modelContextSuffix = regexp.MustCompile(`\[\d+m\]$`)

// shortModelName strips the "claude-" prefix, a trailing context tag and a trailing date suffix.
func shortModelName(raw string) string {
	s := strings.TrimPrefix(raw, "claude-")
	s = modelContextSuffix.ReplaceAllString(s, "")
	s = modelDateSuffix.ReplaceAllString(s, "")
	return s
}

// joinedModelNames renders a phase's model set as sorted short names, '+'-joined when mixed.
func joinedModelNames(models map[string]*modelStats) string {
	if len(models) == 0 {
		return "-"
	}
	return strings.Join(slices.Sorted(maps.Keys(models)), "+")
}

// sortedModelStats orders a model map by cache-read desc, tie-broken by name for stable output.
func sortedModelStats(models map[string]*modelStats) []*modelStats {
	list := make([]*modelStats, 0, len(models))
	for _, ms := range models {
		list = append(list, ms)
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].tokens.CacheReadTokens != list[j].tokens.CacheReadTokens {
			return list[i].tokens.CacheReadTokens > list[j].tokens.CacheReadTokens
		}
		return list[i].model < list[j].model
	})
	return list
}

// printModelRollup renders one MODEL/AGENTS/TURNS/... block, one row per model.
func printModelRollup(out io.Writer, models map[string]*modelStats) {
	if len(models) == 0 {
		return
	}
	head := []string{"MODEL", "AGENTS", "TURNS", "INPUT", "CACHE-READ", "CACHE-CREATE", "OUTPUT"}
	var rows [][]string
	for _, ms := range sortedModelStats(models) {
		rows = append(rows, []string{ms.model, strconv.Itoa(ms.agents), strconv.Itoa(ms.turns),
			strconv.FormatInt(ms.tokens.InputTokens, 10), strconv.FormatInt(ms.tokens.CacheReadTokens, 10),
			strconv.FormatInt(ms.tokens.CacheCreationTokens, 10), strconv.FormatInt(ms.tokens.OutputTokens, 10)})
	}
	fmt.Fprint(out, table.Render(head, nil, rightAlign(head, rows, 1, 6), 0))
}

// rightAlign pads the head and row cells of columns from..to to a common width so numbers stay right-aligned.
func rightAlign(head []string, rows [][]string, from, to int) [][]string {
	for c := from; c <= to; c++ {
		w := utf8.RuneCountInString(head[c])
		for _, r := range rows {
			w = max(w, utf8.RuneCountInString(r[c]))
		}
		for _, r := range append([][]string{head}, rows...) {
			r[c] = strings.Repeat(" ", w-utf8.RuneCountInString(r[c])) + r[c]
		}
	}
	return rows
}

// printAllRunsByModel folds every listed run's total.models together and prints one rollup block,
// keyed by model rather than by run; skipped when there are fewer than two runs, since a single
// run's own per-phase rollup already shows the same totals.
func printAllRunsByModel(out io.Writer, runs []*workflowRun) {
	if len(runs) < 2 {
		return
	}
	all := map[string]*modelStats{}
	for _, run := range runs {
		for name, ms := range run.total.models {
			dst := all[name]
			if dst == nil {
				dst = &modelStats{model: name}
				all[name] = dst
			}
			dst.agents += ms.agents
			dst.turns += ms.turns
			dst.tokens.Add(ms.tokens)
		}
	}
	if len(all) == 0 {
		return
	}
	fmt.Fprintln(out, "\nALL RUNS BY MODEL")
	printModelRollup(out, all)
}

// phaseBefore orders phase rows by start time, pushing timestamp-less rows to the end.
func phaseBefore(a, b *workflowPhaseRow) bool {
	if a.first.IsZero() != b.first.IsZero() {
		return b.first.IsZero()
	}
	if !a.first.Equal(b.first) {
		return a.first.Before(b.first)
	}
	return a.phase < b.phase
}

// workflowNameForRun derives the workflow name from the sibling script <session>/workflows/scripts/
// <name>-<runId>.js; the run id itself carries hyphens, so the exact suffix is stripped, never split.
// That glob only matches inline/by-name runs, so a {scriptPath} run falls back to scanning the
// session's own main transcript for the tool_use that launched it.
func workflowNameForRun(runDir, runID string) string {
	sessionDir := filepath.Dir(filepath.Dir(filepath.Dir(runDir)))
	matches, err := filepath.Glob(filepath.Join(sessionDir, "workflows", "scripts", "*-"+runID+".js"))
	if err == nil && len(matches) > 0 {
		slices.Sort(matches)
		if name := strings.TrimSuffix(filepath.Base(matches[0]), "-"+runID+".js"); name != "" {
			return name
		}
	}
	if name := workflowNamesFromTranscript(sessionDir + ".jsonl")[runID]; name != "" {
		return name
	}
	return runID
}

// toolUseBlock is the subset of an assistant content block's tool_use fields needed to resolve a
// workflow run's name.
type toolUseBlock struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Input struct {
		Name       string `json:"name"`
		ScriptPath string `json:"scriptPath"`
	} `json:"input"`
}

// toolResultBlock is the subset of a user content block's tool_result fields needed to pair it back
// to the tool_use that produced it.
type toolResultBlock struct {
	Type      string          `json:"type"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

// runIDPattern matches a workflow run id following either the "Run ID: " marker or a "Transcript
// dir: .../<runId>" path, the two shapes a Workflow launch result carries it in.
var runIDPattern = regexp.MustCompile(`(?:Run ID: |Transcript dir: \S*/)(wf_\S+)`)

// workflowNameCache caches scanWorkflowNamesFromTranscript's result per session transcript path, so
// several runs under one session don't each rescan it (the file can be large).
var workflowNameCache = map[string]map[string]string{}

// workflowNamesFromTranscript returns runID -> workflow name for every Workflow launch found in the
// session's main transcript, scanning it at most once per process.
func workflowNamesFromTranscript(sessionTranscript string) map[string]string {
	if cached, ok := workflowNameCache[sessionTranscript]; ok {
		return cached
	}
	names := scanWorkflowNamesFromTranscript(sessionTranscript)
	workflowNameCache[sessionTranscript] = names
	return names
}

// scanWorkflowNamesFromTranscript streams the session transcript once, pairing each tool_use with the
// tool_result that names its run id (by tool_use_id), to build the full runID -> name map in one pass.
// Only top-level "assistant"/"user" records are read; api-request-blob duplicates are skipped.
func scanWorkflowNamesFromTranscript(path string) map[string]string {
	names := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return names
	}
	defer f.Close()

	toolUses := map[string]toolUseBlock{}
	_ = transcript.EachLine(f, func(line []byte) {
		var rec struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &rec) != nil {
			return
		}
		switch rec.Type {
		case "assistant":
			indexWorkflowToolUses(rec.Message.Content, toolUses)
		case "user":
			matchWorkflowToolResults(rec.Message.Content, toolUses, names)
		}
	})
	return names
}

func indexWorkflowToolUses(content json.RawMessage, toolUses map[string]toolUseBlock) {
	var blocks []toolUseBlock
	if json.Unmarshal(content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		if b.Type == "tool_use" && b.ID != "" {
			toolUses[b.ID] = b
		}
	}
}

func matchWorkflowToolResults(content json.RawMessage, toolUses map[string]toolUseBlock, names map[string]string) {
	var blocks []toolResultBlock
	if json.Unmarshal(content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		runID := runIDFromToolResult(b.Content)
		if runID == "" || names[runID] != "" {
			continue
		}
		if use, ok := toolUses[b.ToolUseID]; ok {
			if name := workflowNameFromToolUse(use); name != "" {
				names[runID] = name
			}
		}
	}
}

// runIDFromToolResult extracts the wf_<id> run id a tool_result's text names, or "" when none does.
func runIDFromToolResult(raw json.RawMessage) string {
	m := runIDPattern.FindStringSubmatch(transcript.ContentText(raw))
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// workflowNameFromToolUse resolves a Workflow tool_use's name: the by-name input.name, else the
// scriptPath's basename minus .js, else "" (an inline script carries neither).
func workflowNameFromToolUse(use toolUseBlock) string {
	if use.Input.Name != "" {
		return use.Input.Name
	}
	if use.Input.ScriptPath != "" {
		return strings.TrimSuffix(filepath.Base(use.Input.ScriptPath), ".js")
	}
	return ""
}

// printWorkflowRun renders one run's header, its phase rows, the TOTAL row and the per-run model rollup.
func printWorkflowRun(out io.Writer, run *workflowRun) {
	fmt.Fprintf(out, "%s  %s  wall %s\n", run.id, run.name, formatWall(run.total.first, run.total.last))
	fmt.Fprintf(out, "%-20s %7s %7s %12s %12s %12s %12s %10s  %s\n",
		"PHASE", "AGENTS", "TURNS", "INPUT", "CACHE-READ", "CACHE-CREATE", "OUTPUT", "WALL", "MODEL")
	for _, row := range run.phases {
		printWorkflowRow(out, row)
	}
	printWorkflowRow(out, &run.total)
	printModelRollup(out, run.total.models)
}

// printWorkflowRow renders one phase (or TOTAL) row, its MODEL column '+'-joined when mixed.
func printWorkflowRow(out io.Writer, row *workflowPhaseRow) {
	fmt.Fprintf(out, "%-20s %7d %7d %12d %12d %12d %12d %10s  %s\n",
		row.phase, row.agents, row.turns, row.tokens.InputTokens, row.tokens.CacheReadTokens,
		row.tokens.CacheCreationTokens, row.tokens.OutputTokens, formatWall(row.first, row.last),
		joinedModelNames(row.models))
}

// formatWall renders a first→last span; "-" when no record carried a parseable RFC3339 timestamp, so
// an interrupted transcript degrades instead of printing a 1970-based duration.
func formatWall(first, last time.Time) string {
	if first.IsZero() || last.IsZero() || last.Before(first) {
		return "-"
	}
	return last.Sub(first).Round(time.Second).String()
}
