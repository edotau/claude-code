// Package usage is `claude-code usage`: token totals from subagent transcripts, including Workflow-tool agents
// (<session>/subagents/workflows/<runId>/agent-*.jsonl); --workflows rolls each run up per phase, with a
// per-model (MODEL column) split and a per-run plus cross-run model rollup. Live provider plan quotas and the
// local per-token ledger (API-key providers) follow the table (provider_quotas.go); --show picks
// left/used/total for the quota column.
package usage

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/tokenledger"
	"github.com/edotau/claude-code/internal/transcript"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/table"
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
	mode := "subscription" // cli (agent login) and none bill by plan; a static key bills per token
	if sel.Provider.APIKeyAuth() {
		mode = "api"
	}
	return &billing{Mode: mode, Provider: sel.Provider.Name}
}

// document is the `usage --format json` payload; the web page's /api/usage serves the same value.
type document struct {
	*transcript.UsageAggregate
	Providers  []quota      `json:"providers,omitempty"`
	API        []ledgerAgg  `json:"api,omitempty"`
	Billing    *billing     `json:"billing,omitempty"`
	Agents     []agentUsage `json:"agents,omitempty"`
	WindowDays int          `json:"window_days"`
}

// report builds the JSON document for one window (session scopes to one id, else the last days days).
func report(claudeDir, session string, days int, withQuotas bool, errw io.Writer) (*document, error) {
	quotasCh := startQuotaFetch(withQuotas) // overlaps the local scan below
	scan, err := scanUsage(claudeDir, session, days, errw)
	if err != nil {
		return nil, err
	}
	agg := scan.agg
	agents := buildAgentsUsage(scan, session, days)
	quotas := <-quotasCh
	aggs, _ := loadLedger(errw)
	if session != "" {
		days = 0 // --days is ignored with --session, so no window applies
	}
	return &document{agg, quotas, aggs, resolveBilling(), agents, days}, nil
}

// transcriptScan is claude's share of the window: the aggregate and the daily buckets are built from the same
// in-window responses, so they always agree.
type transcriptScan struct {
	agg   *transcript.UsageAggregate
	files []string
	daily []dailyBucket
}

// scanUsage aggregates the transcripts selected by session or days, dropping zero-token model rows. With --days
// only responses stamped inside the window count (a long-lived file is cut per turn); --session counts them all.
func scanUsage(claudeDir, session string, days int, errw io.Writer) (*transcriptScan, error) {
	var files []string
	var err error
	var cutoff time.Time
	if session != "" {
		files, err = usageFilesForSession(claudeDir, session)
	} else {
		cutoff = windowCutoff(days)
		files, err = usageFilesWithinDays(claudeDir, days)
	}
	if err != nil {
		return nil, err
	}
	agg := transcript.NewUsageAggregate()
	counts := map[string]*dayCount{}
	turns := transcript.OpenSummaryCache(transcript.SummaryCachePath())
	for _, f := range files {
		resps, readErr := turns.Responses(f)
		if readErr != nil {
			fmt.Fprintf(errw, "usage: warning: skipping %s: %v\n", f, readErr)
			continue
		}
		kept := foldResponses(f, resps, cutoff, counts)
		agg.Add(transcript.SummarizeResponses(kept))
	}
	_ = turns.Save() // a cache write failure never fails the verb
	// zero-token rows (Claude Code's "<synthetic>" placeholder messages) are noise
	maps.DeleteFunc(agg.Models, func(_ string, u *transcript.ModelUsage) bool { return u.Total() == 0 })
	return &transcriptScan{agg: agg, files: files, daily: dailyFromCounts(counts)}, nil
}

// foldResponses keeps the responses stamped at or after cutoff (a zero cutoff keeps all; an unstamped response
// is dated by its file's mtime) and adds each token-bearing one to its local day in counts.
func foldResponses(path string, resps []transcript.Response, cutoff time.Time, counts map[string]*dayCount) []transcript.Response {
	var mtimeDay string
	kept := inWindowResponses(resps, cutoff)
	for _, r := range kept {
		if r.Usage.Total() == 0 {
			continue
		}
		day := r.At.Local().Format("2006-01-02")
		if r.At.IsZero() {
			if mtimeDay == "" {
				if info, err := os.Stat(path); err == nil {
					mtimeDay = info.ModTime().Local().Format("2006-01-02")
				}
			}
			day = mtimeDay
		}
		if day == "" {
			continue
		}
		addDayCount(counts, day, int(r.Usage.Total()), 1)
	}
	return kept
}

// usageOpts are the validated `usage` flags.
type usageOpts struct {
	session      string
	days         int
	show         string
	jsonOut      bool
	workflows    bool
	wantProvider bool
}

// parseUsageArgs parses and validates the flags; done is true when the caller should exit with code.
func parseUsageArgs(args []string, errw io.Writer) (o usageOpts, code int, done bool) {
	fs := helptext.NewFlagSet("usage", helptext.Page{
		Usage: "claude-code usage [flags]",
		About: "Token totals and peak input tokens/min across Claude Code subagent transcripts and the " +
			"codex, opencode and copilot stores; then live plan quotas, API-key spend and a per-agent table.",
	})
	fs.SetOutput(errw)
	sessionFlag := fs.String("session", "", "scope to one session id (every project dir is searched)")
	daysFlag := fs.Int("days", 1, "count the last N days (ignored with --session)")
	formatFlag := helptext.FormatFlag(fs)
	workflowsFlag := fs.Bool("workflows", false, "workflow phases and model rollups, newest first; text only")
	providersFlag := fs.Bool("providers", true, "append live plan quotas for every provider; =false stays offline")
	showFlag := fs.String("show", "used", "quota column: used (default) or left; total hides the quota table")
	if code, done := helptext.Parse(fs, args); done {
		return usageOpts{}, code, true
	}
	raw := usageOpts{session: *sessionFlag, days: *daysFlag, show: *showFlag, workflows: *workflowsFlag, wantProvider: *providersFlag}
	o, err := buildUsageOpts(raw, *formatFlag)
	if err != nil {
		fmt.Fprintln(errw, "usage:", err)
		return usageOpts{}, 2, true
	}
	return o, 0, false
}

// buildUsageOpts validates the raw flag values; every refusal is an exit-2 usage error.
func buildUsageOpts(o usageOpts, format string) (usageOpts, error) {
	if err := helptext.CheckFormat(format); err != nil {
		return usageOpts{}, err
	}
	if !slices.Contains([]string{"left", "used", "total"}, o.show) {
		return usageOpts{}, fmt.Errorf("--show must be left, used, or total (got %q)", o.show)
	}
	o.jsonOut = format == helptext.JSON
	if o.workflows && o.jsonOut {
		return usageOpts{}, errors.New("--workflows has no JSON form; drop --format json (the per-model aggregate does)")
	}
	if o.days <= 0 {
		return usageOpts{}, fmt.Errorf("--days must be 1 or more (got %d); use --days 1 for the last day", o.days)
	}
	o.wantProvider = o.wantProvider && o.show != "total"
	return o, nil
}

// Run is `claude-code usage`; claudeDir is injectable so tests never touch the real ~/.claude/projects tree.
func Run(args []string, claudeDir string, out, errw io.Writer) int {
	o, code, done := parseUsageArgs(args, errw)
	if done {
		return code
	}
	if o.workflows {
		return runUsageWorkflows(claudeDir, out, errw, o.session, o.days)
	}
	if o.jsonOut {
		doc, err := report(claudeDir, o.session, o.days, o.wantProvider, errw)
		if err != nil {
			fmt.Fprintln(errw, "usage:", err)
			return 1
		}
		if err := helptext.WriteJSON(out, doc); err != nil {
			return 1
		}
		return 0
	}
	return runUsageText(o, claudeDir, out, errw)
}

// runUsageText prints the five text sections, overlapping the quota fetch and agent aggregation with the local scan.
func runUsageText(o usageOpts, claudeDir string, out, errw io.Writer) int {
	session, days, show := o.session, o.days, o.show
	// Launched now so the live network round-trips overlap the local scan/aggregation below instead of
	// following them; startQuotaFetch yields an empty result immediately when quotas aren't wanted.
	quotasCh := startQuotaFetch(o.wantProvider)

	scan, err := scanUsage(claudeDir, session, days, errw)
	if err != nil {
		fmt.Fprintln(errw, "usage:", err)
		return 1
	}
	agg, files := scan.agg, scan.files
	w := &textutil.ErrWriter{W: out}

	// Section 1 (MODEL table) prints the moment the local scan is done, even while quotas are in flight.
	if len(files) == 0 {
		fmt.Fprintln(errw, "usage: no matching subagent transcripts found")
	} else {
		printUsageTable(w, agg, len(files))
	}

	// Agents aggregation depends on agg/files above, so it starts here, overlapping the quota fetch too.
	agentsCh := make(chan []agentUsage, 1)
	go func() {
		agentsCh <- buildAgentsUsage(scan, session, days)
	}()

	// Section 2 (PROVIDER QUOTAS) + section 3 (API KEYS): wait for the quota fetch, then print both.
	quotas := <-quotasCh
	aggs, ledgerEntries := loadLedger(errw)
	printQuotas(w, quotas, show, aggs, ledgerSince(ledgerEntries))

	// Section 4 (AGENTS): the aggregation above has had the whole quota round-trip to finish by now.
	printAgentsSection(w, <-agentsCh, session, days)
	if w.Err != nil {
		fmt.Fprintln(errw, "usage: write output:", w.Err)
		return 1
	}

	// Section 5: the warn/exceeded trailer, derived from the same quotas already printed.
	printQuotaWarnings(errw, quotas)
	return 0
}

// loadLedger reads the ledger (warning on errw when unreadable) and returns its aggregates plus the raw entries.
func loadLedger(errw io.Writer) ([]ledgerAgg, []tokenledger.Entry) {
	entries, err := readLedger()
	if err != nil {
		fmt.Fprintln(errw, "usage: warning: ledger unreadable:", err)
	}
	return aggregateLedger(entries, time.Now()), entries
}

// windowCutoff is the start of a trailing window of the given days.
func windowCutoff(days int) time.Time {
	return time.Now().Add(-time.Duration(days) * 24 * time.Hour)
}

// startQuotaFetch runs the live quota fetch on its own goroutine, overlapping the local scan; empty at once when off.
func startQuotaFetch(enabled bool) <-chan []quota {
	ch := make(chan []quota, 1)
	if !enabled {
		ch <- nil
		return ch
	}
	go func() {
		qs := fetchQuotas()
		for i := range qs {
			if !noteOnly(qs[i]) {
				qs[i].LeftPct = max(100-qs[i].UsedPct, 0)
			}
		}
		setStates(qs, warnThreshold())
		recordQuotaState(qs)
		ch <- qs
	}()
	return ch
}

// readLedger is the ledger read seam; a var so tests can stub it without touching the real ledger file.
var readLedger = tokenledger.Read

// usageFilesForSession globs every project dir for <id>/subagents/*.jsonl and the workflow agents at
// <id>/subagents/workflows/*/agent-*.jsonl — a session id is unique but its parent project-dir
// encoding isn't known to the caller, so every project dir is tried.
func usageFilesForSession(claudeDir, id string) ([]string, error) {
	pattern := filepath.Join(claudeDir, "projects", "*", id, "subagents", "*.jsonl")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("usage: glob %s: %w", pattern, err)
	}
	for _, run := range transcript.WorkflowRunDirs(filepath.Join(claudeDir, "projects", "*", id)) {
		matches = append(matches, transcript.WorkflowAgentFiles(run)...)
	}
	return dedupeFiles(matches), nil
}

// usageFilesWithinDays walks every session dir's subagents/*.jsonl plus its workflow agents, keeping
// only files modified within the last N days: a session dir that was merely touched recently contributes
// just its fresh transcripts, not its whole history. The dir gate (newest mtime under it) bounds the walk.
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
		for _, run := range transcript.WorkflowRunDirs(dir) {
			files = append(files, transcript.WorkflowAgentFiles(run)...)
		}
	}
	cutoff := windowCutoff(days)
	files = slices.DeleteFunc(files, func(f string) bool {
		info, statErr := os.Stat(f)
		return statErr != nil || info.ModTime().Before(cutoff)
	})
	return dedupeFiles(files), nil
}

// usageSessionDirsWithinDays lists the projects/*/<session> dirs modified within the last N days.
func usageSessionDirsWithinDays(claudeDir string, days int) ([]string, error) {
	pattern := filepath.Join(claudeDir, "projects", "*", "*")
	candidates, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("usage: glob %s: %w", pattern, err)
	}
	cutoff := windowCutoff(days)

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
// totals, so one file is never summarized twice.
func dedupeFiles(files []string) []string {
	slices.Sort(files)
	return slices.Compact(files)
}

// printUsageTable renders per-model totals sorted by name, plus the peak input tokens/min and the file count scanned.
func printUsageTable(out io.Writer, agg *transcript.UsageAggregate, fileCount int) {
	models := slices.Sorted(maps.Keys(agg.Models))

	n := func(v int64) string { return fmt.Sprintf("%12d", v) }
	head := []string{"MODEL", fmt.Sprintf("%12s", "INPUT"), fmt.Sprintf("%12s", "OUTPUT"),
		fmt.Sprintf("%12s", "CACHE-READ"), fmt.Sprintf("%12s", "CACHE-CREATE"), fmt.Sprintf("%12s", "TOTAL")}
	rows := make([][]string, 0, len(models))
	for _, m := range models {
		u := agg.Models[m]
		rows = append(rows, []string{m, n(u.InputTokens), n(u.OutputTokens), n(u.CacheReadTokens), n(u.CacheCreationTokens), n(u.Total())})
	}
	fmt.Fprint(out, table.Render(head, nil, rows, 0))
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
		info, err := d.Info() // unknown mtime could be in window, so it is an error, not a skip
		if err != nil {
			return err
		}
		if info.ModTime().After(newest) {
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

// Spec is the completion node for the usage verb.
func Spec() *helptext.Node {
	flag, boolFlag := helptext.Plain, helptext.BoolFlag
	return &helptext.Node{Flags: []helptext.Flag{flag("session"), flag("days"), helptext.CompletionFormatFlag(), boolFlag("workflows"), boolFlag("providers"), flag("show")}}
}
