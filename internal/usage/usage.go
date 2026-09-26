// Package usage is `claude-code usage`: token totals from subagent transcripts, including Workflow-tool agents
// (<session>/subagents/workflows/<runId>/agent-*.jsonl); --workflows rolls each run up per phase.
package usage

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/transcript"
)

// Run is `claude-code usage`; claudeDir is injectable so tests never touch the real ~/.claude/projects tree.
func Run(args []string, claudeDir string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	fs.SetOutput(errw)
	sessionFlag := fs.String("session", "", "scope to one session id (every project dir is searched)")
	daysFlag := fs.Int("days", 1, "scope to sessions modified within the last N days (ignored with --session)")
	jsonFlag := fs.Bool("json", false, "emit JSON instead of a table")
	workflowsFlag := fs.Bool("workflows", false, "one per-phase block per Workflow run, newest first")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	session, days, jsonOut, workflowsOut := *sessionFlag, *daysFlag, *jsonFlag, *workflowsFlag
	if workflowsOut && jsonOut {
		fmt.Fprintln(errw, "usage: --workflows has no JSON form; drop --json (the per-model aggregate does)")
		return 2
	}
	if workflowsOut {
		return runUsageWorkflows(claudeDir, out, errw, session, days)
	}

	var files []string
	var err error
	if session != "" {
		files, err = usageFilesForSession(claudeDir, session)
	} else {
		if days <= 0 {
			days = 1
		}
		files, err = usageFilesWithinDays(claudeDir, days)
	}
	if err != nil {
		fmt.Fprintln(errw, "usage:", err)
		return 1
	}
	if len(files) == 0 {
		fmt.Fprintln(errw, "usage: no matching subagent transcripts found")
		return 0
	}

	agg := transcript.NewUsageAggregate()
	for _, f := range files {
		if addErr := agg.AddFile(f); addErr != nil {
			fmt.Fprintf(errw, "usage: warning: skipping %s: %v\n", f, addErr)
		}
	}

	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(agg); err != nil {
			return 1
		}
		return 0
	}
	printUsageTable(out, agg, len(files))
	return 0
}

// usageFilesForSession globs every project dir for <id>/subagents/*.jsonl and the workflow agents at
// <id>/subagents/workflows/*/agent-*.jsonl — a session id is unique but its parent project-dir
// encoding isn't known to the caller, so every project dir is tried.
func usageFilesForSession(claudeDir, id string) ([]string, error) {
	pattern := filepath.Join(claudeDir, "projects", "*", id, "subagents", "*.jsonl")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("usage: glob %s: %w", pattern, err)
	}
	wfPattern := filepath.Join(claudeDir, "projects", "*", id, "subagents", "workflows", "*", "agent-*.jsonl")
	wf, wfErr := filepath.Glob(wfPattern)
	if wfErr != nil {
		return nil, fmt.Errorf("usage: glob %s: %w", wfPattern, wfErr)
	}
	return dedupeFiles(append(matches, wf...)), nil
}

// usageFilesWithinDays walks every session dir's subagents/*.jsonl plus its workflow agents, keeping
// only sessions whose own modtime falls within the last N days — bounds the walk instead of scanning
// everything. The gate is the SESSION dir's mtime, so a workflow run under an untouched session dir
// is excluded even when the run itself is fresh.
func usageFilesWithinDays(claudeDir string, days int) ([]string, error) {
	sessionDirs, err := usageSessionDirsWithinDays(claudeDir, days)
	if err != nil {
		return nil, err
	}

	var files []string
	for _, dir := range sessionDirs {
		matches, globErr := filepath.Glob(filepath.Join(dir, "subagents", "*.jsonl"))
		if globErr == nil {
			files = append(files, matches...)
		}
		for _, run := range workflowRunDirs(dir) {
			files = append(files, workflowAgentFiles(run)...)
		}
	}
	return dedupeFiles(files), nil
}

// usageSessionDirsWithinDays lists the projects/*/<session> dirs modified within the last N days.
func usageSessionDirsWithinDays(claudeDir string, days int) ([]string, error) {
	pattern := filepath.Join(claudeDir, "projects", "*", "*")
	candidates, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("usage: glob %s: %w", pattern, err)
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)

	var dirs []string
	for _, dir := range candidates {
		info, statErr := os.Stat(dir)
		if statErr != nil || !info.IsDir() || info.ModTime().Before(cutoff) {
			continue
		}
		dirs = append(dirs, dir)
	}
	slices.Sort(dirs)
	return dirs, nil
}

// dedupeFiles sorts and uniques the scanned-file list — it drives `files scanned` as well as the
// totals, so one path reaching AddFile twice would silently double a model's tokens.
func dedupeFiles(files []string) []string {
	slices.Sort(files)
	return slices.Compact(files)
}

// printUsageTable renders per-model totals sorted by name, plus the peak-concurrency + rate-limit
// figures and the file count scanned.
func printUsageTable(out io.Writer, agg *transcript.UsageAggregate, fileCount int) {
	models := slices.Sorted(maps.Keys(agg.Models))

	fmt.Fprintf(out, "%-24s %12s %12s %12s %12s %12s\n", "MODEL", "INPUT", "OUTPUT", "CACHE-READ", "CACHE-CREATE", "TOTAL")
	for _, m := range models {
		u := agg.Models[m]
		fmt.Fprintf(out, "%-24s %12d %12d %12d %12d %12d\n",
			m, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheCreationTokens, u.Total())
	}
	fmt.Fprintf(out, "\nfiles scanned:              %d\n", fileCount)
	fmt.Fprintf(out, "peak input tokens/min:      %d  (uncached input + cache writes; the ITPM 429-risk proxy)\n", agg.PeakInputTokensPerMin)
}
