package usage

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/transcript"
)

// workflowPhaseRow is one phase's agents folded together inside a single run.
type workflowPhaseRow struct {
	phase  string
	agents int
	turns  int
	tokens transcript.ModelUsage
	first  time.Time
	last   time.Time
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

	var runs []*workflowRun
	scanned := 0
	for _, dir := range runDirs {
		run, files := loadWorkflowRun(dir, errw)
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
	for i, run := range runs {
		if i > 0 {
			fmt.Fprintln(out)
		}
		printWorkflowRun(out, run)
	}
	fmt.Fprintf(out, "\nworkflow runs: %d   agent transcripts scanned: %d\n", len(runs), scanned)
	return 0
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
		pattern := filepath.Join(claudeDir, "projects", "*", session, "subagents", "workflows", "*")
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("glob %s: %w", pattern, err)
		}
		return keepDirs(matches), nil
	}
	if days <= 0 {
		days = 1
	}
	sessionDirs, err := usageSessionDirsWithinDays(claudeDir, days)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, dir := range sessionDirs {
		dirs = append(dirs, workflowRunDirs(dir)...)
	}
	return dirs, nil
}

// workflowRunDirs lists one session's workflows/<runId> dirs.
func workflowRunDirs(sessionDir string) []string {
	matches, err := filepath.Glob(filepath.Join(sessionDir, "subagents", "workflows", "*"))
	if err != nil {
		return nil
	}
	return keepDirs(matches)
}

// keepDirs filters a glob result down to directories, sorted.
func keepDirs(matches []string) []string {
	var dirs []string
	for _, m := range matches {
		if info, err := os.Stat(m); err == nil && info.IsDir() {
			dirs = append(dirs, m)
		}
	}
	slices.Sort(dirs)
	return dirs
}

// workflowAgentFiles lists one run dir's agent transcripts; the agent-*.jsonl shape keeps the sibling
// *.meta.json sidecars out of the token scan.
func workflowAgentFiles(runDir string) []string {
	matches, err := filepath.Glob(filepath.Join(runDir, "agent-*.jsonl"))
	if err != nil {
		return nil
	}
	slices.Sort(matches)
	return matches
}

// loadWorkflowRun folds every agent transcript in one run dir into phase rows, returning the run and
// the number of transcripts scanned (nil when the dir holds none).
func loadWorkflowRun(runDir string, errw io.Writer) (*workflowRun, int) {
	files := workflowAgentFiles(runDir)
	if len(files) == 0 {
		return nil, 0
	}
	run := &workflowRun{id: filepath.Base(runDir)}
	run.name = workflowNameForRun(runDir, run.id)
	run.total.phase = "TOTAL"
	if info, err := os.Stat(runDir); err == nil {
		run.mtime = info.ModTime()
	}

	byPhase := map[string]*workflowPhaseRow{}
	scanned := 0
	for _, f := range files {
		summary, err := transcript.SummarizeFile(f)
		if err != nil {
			fmt.Fprintf(errw, "usage: warning: skipping %s: %v\n", f, err)
			continue
		}
		scanned++
		phase := workflowPhaseFromMeta(f, errw)
		if phase == "" {
			phase = "(none)"
		}
		row := byPhase[phase]
		if row == nil {
			row = &workflowPhaseRow{phase: phase}
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

// addAgentToRow folds one agent transcript's summary into a phase (or run-total) row.
func addAgentToRow(row *workflowPhaseRow, s *transcript.FileSummary) {
	t := s.Totals()
	row.agents++
	row.turns += s.Turns
	row.tokens.InputTokens += t.InputTokens
	row.tokens.OutputTokens += t.OutputTokens
	row.tokens.CacheReadTokens += t.CacheReadTokens
	row.tokens.CacheCreationTokens += t.CacheCreationTokens
	if !s.First.IsZero() && (row.first.IsZero() || s.First.Before(row.first)) {
		row.first = s.First
	}
	if s.Last.After(row.last) {
		row.last = s.Last
	}
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

// workflowPhaseFromMeta reads the sibling agent-*.meta.json's workflowPhase; plenty of real metas
// carry none, so "" is a normal answer routed to `(none)` .
func workflowPhaseFromMeta(transcriptPath string, errw io.Writer) string {
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

// workflowNameForRun derives the workflow name from the sibling script <session>/workflows/scripts/
// <name>-<runId>.js; the run id itself carries hyphens, so the exact suffix is stripped, never split.
func workflowNameForRun(runDir, runID string) string {
	sessionDir := filepath.Dir(filepath.Dir(filepath.Dir(runDir)))
	matches, err := filepath.Glob(filepath.Join(sessionDir, "workflows", "scripts", "*-"+runID+".js"))
	if err != nil || len(matches) == 0 {
		return runID
	}
	slices.Sort(matches)
	name := strings.TrimSuffix(filepath.Base(matches[0]), "-"+runID+".js")
	if name == "" {
		return runID
	}
	return name
}

// printWorkflowRun renders one run's header, its phase rows and the TOTAL row.
func printWorkflowRun(out io.Writer, run *workflowRun) {
	fmt.Fprintf(out, "%s  %s  wall %s\n", run.id, run.name, formatWall(run.total.first, run.total.last))
	fmt.Fprintf(out, "%-20s %7s %7s %12s %12s %12s %12s %10s\n",
		"PHASE", "AGENTS", "TURNS", "INPUT", "CACHE-READ", "CACHE-CREATE", "OUTPUT", "WALL")
	for _, row := range run.phases {
		printWorkflowRow(out, row)
	}
	printWorkflowRow(out, &run.total)
}

// printWorkflowRow renders one phase (or TOTAL) row.
func printWorkflowRow(out io.Writer, row *workflowPhaseRow) {
	fmt.Fprintf(out, "%-20s %7d %7d %12d %12d %12d %12d %10s\n",
		row.phase, row.agents, row.turns, row.tokens.InputTokens, row.tokens.CacheReadTokens,
		row.tokens.CacheCreationTokens, row.tokens.OutputTokens, formatWall(row.first, row.last))
}

// formatWall renders a first→last span; "-" when no record carried a parseable RFC3339 timestamp, so
// an interrupted transcript degrades instead of printing a 1970-based duration.
func formatWall(first, last time.Time) string {
	if first.IsZero() || last.IsZero() || last.Before(first) {
		return "-"
	}
	return last.Sub(first).Round(time.Second).String()
}
