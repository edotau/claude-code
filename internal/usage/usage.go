// Package usage is `claude-code usage`: token totals from subagent transcripts, including Workflow-tool agents
// (<session>/subagents/workflows/<runId>/agent-*.jsonl); --workflows rolls each run up per phase, with a
// per-model (MODEL column) split and a per-run plus cross-run model rollup. Live provider plan quotas and the
// local per-token ledger (API-key providers; ledger.go) follow the table (providers.go); --show picks
// left/used/total for the quota column.
package usage

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/transcript"
	"github.com/edotau/claude-code/internal/usageledger"
)

// billing names the session provider's pricing: a Claude.ai plan quota, or per-token API billing.
type billing struct {
	Mode     string `json:"mode"`
	Provider string `json:"provider"`
}

// resolveBilling reports "subscription" unless the session provider authenticates with an API key.
func resolveBilling() *billing {
	reg, err := providers.Load()
	if err != nil {
		return nil
	}
	sel, err := providers.Select(reg, "")
	if err != nil || sel.Provider == nil {
		return nil
	}
	mode := "subscription" // passthrough (vendor login) and none bill by plan; a static key bills per token
	if t := sel.Provider.Auth.Type; t == providers.AuthAPIKey || t == providers.AuthBearer {
		mode = "api"
	}
	return &billing{Mode: mode, Provider: sel.Provider.Name}
}

// Run is `claude-code usage`; claudeDir is injectable so tests never touch the real ~/.claude/projects tree.
func Run(args []string, claudeDir string, out, errw io.Writer) int {
	fs := helptext.NewFlagSet("usage", helptext.Page{
		Usage: "claude-code usage [flags]",
		About: "Token totals and peak rate across Claude Code sessions plus the codex, opencode\n" +
			"and copilot stores, then live plan quotas per provider.",
	})
	fs.SetOutput(errw)
	sessionFlag := fs.String("session", "", "scope to one session id (every project dir is searched)")
	daysFlag := fs.Int("days", 1, "select sessions modified in N days (ignored with --session)")
	formatFlag := outfmt.Flag(fs)
	workflowsFlag := fs.Bool("workflows", false, "show workflow phases and model rollups, newest runs first")
	providersFlag := fs.Bool("providers", true, "append live plan quotas (GitHub Copilot, OpenAI, Google); =false stays offline")
	showFlag := fs.String("show", "used", "quota column: used (default), left, or total")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := outfmt.Check(*formatFlag); err != nil {
		fmt.Fprintln(errw, "usage:", err)
		return 2
	}
	show := *showFlag
	if show != "left" && show != "used" && show != "total" {
		fmt.Fprintf(errw, "usage: --show must be left, used, or total (got %q)\n", *showFlag)
		return 2
	}
	session, days, jsonOut, workflowsOut := *sessionFlag, *daysFlag, *formatFlag == outfmt.JSON, *workflowsFlag
	if workflowsOut && jsonOut {
		fmt.Fprintln(errw, "usage: --workflows has no JSON form; drop --format json (the per-model aggregate does)")
		return 2
	}
	if workflowsOut {
		return runUsageWorkflows(claudeDir, out, errw, session, days)
	}

	// Launched now so the live network round-trips overlap the local scan/aggregation below instead of
	// following them; startQuotaFetch yields an empty result immediately when quotas aren't wanted.
	quotasCh := startQuotaFetch(*providersFlag && show != "total")

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
	agg := transcript.NewUsageAggregate()
	for _, f := range files {
		if addErr := agg.AddFile(f); addErr != nil {
			fmt.Fprintf(errw, "usage: warning: skipping %s: %v\n", f, addErr)
		}
	}
	// zero-token rows (Claude Code's "<synthetic>" placeholder messages) are noise
	maps.DeleteFunc(agg.Models, func(_ string, u *transcript.ModelUsage) bool { return u.Total() == 0 })

	if jsonOut {
		agents := buildAgentsUsage(agg, claudeSessionCount(claudeDir, session, days, len(files)), days, files)
		quotas := <-quotasCh
		ledgerEntries, ledgerErr := readLedger()
		if ledgerErr != nil {
			fmt.Fprintln(errw, "usage: warning: ledger unreadable:", ledgerErr)
		}
		aggs := aggregateLedger(ledgerEntries, time.Now())
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			*transcript.UsageAggregate
			Providers  []quota      `json:"providers,omitempty"`
			API        []ledgerAgg  `json:"api,omitempty"`
			Billing    *billing     `json:"billing,omitempty"`
			Agents     []agentUsage `json:"agents,omitempty"`
			WindowDays int          `json:"window_days"`
		}{agg, quotas, aggs, resolveBilling(), agents, days}); err != nil {
			return 1
		}
		return 0
	}

	// Section 1 (MODEL table) prints the moment the local scan is done, even while quotas are in flight.
	if len(files) == 0 {
		fmt.Fprintln(errw, "usage: no matching subagent transcripts found")
	} else {
		printUsageTable(out, agg, len(files))
	}

	// Agents aggregation depends on agg/files above, so it starts here, overlapping the quota fetch too.
	agentsCh := make(chan []agentUsage, 1)
	go func() {
		agentsCh <- buildAgentsUsage(agg, claudeSessionCount(claudeDir, session, days, len(files)), days, files)
	}()

	// Section 2 (PROVIDER QUOTAS) + section 3 (API KEYS): wait for the quota fetch, then print both.
	quotas := <-quotasCh
	ledgerEntries, ledgerErr := readLedger()
	if ledgerErr != nil {
		fmt.Fprintln(errw, "usage: warning: ledger unreadable:", ledgerErr)
	}
	aggs := aggregateLedger(ledgerEntries, time.Now())
	printQuotas(out, quotas, show, aggs, ledgerSince(ledgerEntries))

	// Section 4 (AGENTS): the aggregation above has had the whole quota round-trip to finish by now.
	printAgentsSection(out, <-agentsCh, days)

	// Section 5: the warn/exceeded trailer, derived from the same quotas already printed.
	printQuotaWarnings(errw, quotas)
	return 0
}

// startQuotaFetch launches the live provider-quota fetch on its own goroutine so it overlaps the local
// transcript scan and agent aggregation; the channel is buffered so the caller never blocks on send.
// When quotas aren't wanted it sends an empty result immediately, so callers never branch on a nil channel.
func startQuotaFetch(enabled bool) <-chan []quota {
	ch := make(chan []quota, 1)
	if !enabled {
		ch <- nil
		return ch
	}
	go func() {
		qs := fetchQuotas()
		for i := range qs {
			qs[i].LeftPct = 100 - qs[i].UsedPct
		}
		setStates(qs, warnThreshold())
		recordQuotaState(qs)
		ch <- qs
	}()
	return ch
}

// readLedger is the ledger read seam; a var so tests can stub it without touching the real ledger file.
var readLedger = usageledger.Read

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
		if statErr != nil || !info.IsDir() {
			continue
		}
		mtime, mErr := newestModTime(dir)
		if mErr != nil {
			return nil, fmt.Errorf("usage: %w", mErr)
		}
		if mtime.Before(cutoff) {
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

// newestModTime is the latest mtime of dir or anything under it: appending to a transcript in subagents/ leaves
// the session dir's own mtime untouched, so a resumed session would otherwise fall outside --days. A walk
// error (e.g. an unreadable subdir) propagates instead of returning a partial, too-old timestamp that would
// silently drop the session from --days.
func newestModTime(dir string) (time.Time, error) {
	var newest time.Time
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("walk %s: %w", dir, err)
	}
	return newest, nil
}

// printQuotaWarnings prints warn/exceeded rows to errw.
func printQuotaWarnings(errw io.Writer, qs []quota) {
	for _, q := range qs {
		if q.State == "warn" || q.State == "exceeded" {
			account := q.Account
			if account == "" {
				account = "-"
			}
			fmt.Fprintf(errw, "usage: %s %s at %.0f%% (%s)\n", q.Provider, account, q.UsedPct, q.State)
		}
	}
}
