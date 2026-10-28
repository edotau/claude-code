// agents.go — per-agent-CLI usage (claude/codex/opencode/copilot) for the "agents" JSON section and the
// AGENTS text table: each reads its own CLI's local store, independent of the transcript-based table above.
package usage

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/transcript"
)

// agentModel is one provider+model row under an agentUsage.
type agentModel struct {
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	Turns    int     `json:"turns"`
	Tokens   int     `json:"tokens"`
	CostUSD  float64 `json:"cost_usd,omitempty"`
}

// agentUsage is one agent CLI's rollup for the usage window.
type agentUsage struct {
	Agent      string        `json:"agent"`
	Sessions   int           `json:"sessions"`
	Turns      int           `json:"turns"`
	Tokens     int           `json:"tokens"`
	CostUSD    float64       `json:"cost_usd,omitempty"`
	Models     []agentModel  `json:"models,omitempty"`
	Mix        *tokenMix     `json:"mix,omitempty"`
	Daily      []dailyBucket `json:"daily,omitempty"`
	ActiveDays int           `json:"active_days,omitempty"`
	Note       string        `json:"note,omitempty"`
}

// tokenMix splits an agent's window totals across input/output/cache-read/cache-write.
type tokenMix struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cache_read"`
	CacheWrite int `json:"cache_write"`
}

// add folds another mix in, in place.
func (m *tokenMix) add(o tokenMix) {
	m.Input += o.Input
	m.Output += o.Output
	m.CacheRead += o.CacheRead
	m.CacheWrite += o.CacheWrite
}

// mixPtr returns nil for an all-zero mix (the JSON field is then omitted), else a pointer to a copy.
func mixPtr(m tokenMix) *tokenMix {
	if m == (tokenMix{}) {
		return nil
	}
	return &m
}

// dailyBucket is one local day's totals for an agentUsage's Daily activity series.
type dailyBucket struct {
	Date   string `json:"date"`
	Tokens int    `json:"tokens"`
	Turns  int    `json:"turns"`
}

// dayCount is the mutable accumulator dailyFromCounts sorts into []dailyBucket.
type dayCount struct{ tokens, turns int }

// dailyFromCounts turns a date->totals map into an ascending, date-sorted []dailyBucket.
func dailyFromCounts(counts map[string]*dayCount) []dailyBucket {
	dates := make([]string, 0, len(counts))
	for d := range counts {
		dates = append(dates, d)
	}
	slices.Sort(dates)
	out := make([]dailyBucket, 0, len(dates))
	for _, d := range dates {
		c := counts[d]
		out = append(out, dailyBucket{Date: d, Tokens: c.tokens, Turns: c.turns})
	}
	return out
}

// claudeSessionCount is the number of session dirs the verb's own file scan matched: one for an explicit
// --session with any files, else the count usageFilesWithinDays already walked for the --days window.
func claudeSessionCount(claudeDir, session string, days, filesFound int) int {
	if session != "" {
		if filesFound > 0 {
			return 1
		}
		return 0
	}
	dirs, err := usageSessionDirsWithinDays(claudeDir, days)
	if err != nil {
		return 0
	}
	return len(dirs)
}

// buildAgentsUsage runs the codex/opencode/copilot store reads concurrently (10s budget) and folds the
// already-computed claude transcript aggregate in; always returns all four agents, in this order.
// claudeFiles is the same file list the caller already fed into claudeAgg, re-walked here (reusing
// transcript.SummarizeFile, not a second parser) to bucket claude's totals per day.
func buildAgentsUsage(claudeAgg *transcript.UsageAggregate, claudeSessions, days int, claudeFiles []string) []agentUsage {
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var codexU, opencodeU, copilotU agentUsage
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); codexU = codexUsage(cutoff) }()
	go func() { defer wg.Done(); opencodeU = opencodeUsage(ctx, cutoff) }()
	go func() { defer wg.Done(); copilotU = copilotUsage(ctx, cutoff) }()
	wg.Wait()

	return []agentUsage{claudeAgentUsage(claudeAgg, claudeSessions, claudeFiles), codexU, opencodeU, copilotU}
}

// sortModelsByTokens orders a model slice by tokens descending, in place.
func sortModelsByTokens(models []agentModel) {
	slices.SortFunc(models, func(a, b agentModel) int { return cmp.Compare(b.Tokens, a.Tokens) })
}

// claudeAgentUsage folds the verb's own transcript aggregate (already scoped to the window) into an
// agentUsage row; per-model turn counts aren't tracked at the aggregate level, so Turns stays 0.
func claudeAgentUsage(agg *transcript.UsageAggregate, sessions int, files []string) agentUsage {
	if agg == nil || len(agg.Models) == 0 {
		return agentUsage{Agent: "claude", Note: "no sessions in window"}
	}
	var total int
	var mix tokenMix
	models := make([]agentModel, 0, len(agg.Models))
	for name, u := range agg.Models {
		tok := int(u.Total())
		total += tok
		models = append(models, agentModel{Provider: "anthropic", Model: name, Tokens: tok})
		mix.add(tokenMix{Input: int(u.InputTokens), Output: int(u.OutputTokens),
			CacheRead: int(u.CacheReadTokens), CacheWrite: int(u.CacheCreationTokens)})
	}
	sortModelsByTokens(models)
	daily := claudeDailyFromFiles(files)
	return agentUsage{Agent: "claude", Sessions: sessions, Tokens: total, Models: models,
		Mix: mixPtr(mix), Daily: daily, ActiveDays: len(daily)}
}

// claudeDailyFromFiles re-summarizes each matched transcript file — reusing transcript.SummarizeFile,
// never a second parser — and buckets its totals on the file's mtime day, local time.
func claudeDailyFromFiles(files []string) []dailyBucket {
	counts := map[string]*dayCount{}
	for _, f := range files {
		info, statErr := os.Stat(f)
		if statErr != nil {
			continue
		}
		s, sumErr := transcript.SummarizeFile(f)
		if sumErr != nil {
			continue
		}
		tot := s.Totals()
		if tot.Total() == 0 && s.Turns == 0 {
			continue
		}
		day := info.ModTime().Local().Format("2006-01-02")
		c := counts[day]
		if c == nil {
			c = &dayCount{}
			counts[day] = c
		}
		c.tokens += int(tot.Total())
		c.turns += s.Turns
	}
	return dailyFromCounts(counts)
}

// codexHome honors CODEX_HOME, else ~/.codex.
func codexHome() (string, error) {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

// codexEventPayload mirrors a codex session line's payload: turn_context carries the model, event_msg
// carries token_count with the cumulative total_token_usage for the session so far; Timestamp is the
// line's own RFC3339 clock, used to date a session by its LAST token_count line.
type codexLine struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}
type codexTurnContext struct {
	Model string `json:"model"`
}
type codexTokenUsage struct {
	TotalTokens           int `json:"total_tokens"`
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	CacheWriteInputTokens int `json:"cache_write_input_tokens"`
}
type codexEventMsg struct {
	Type string `json:"type"`
	Info struct {
		TotalTokenUsage codexTokenUsage `json:"total_token_usage"`
	} `json:"info"`
}

// codexSessionSummary is one parsed rollout file: model/tokens/turns as before, plus the token-kind
// Mix and the local calendar Day of the last token_count line (empty when no line parsed as RFC3339).
type codexSessionSummary struct {
	Model  string
	Tokens int
	Turns  int
	Mix    tokenMix
	Day    string
}

// parseCodexSession streams one session jsonl: model is the last turn_context seen, tokens/mix come from
// the last token_count's cumulative totals (not summed — each line already totals the whole session),
// turns counts the token_count lines, Day is that last line's timestamp. Malformed lines are skipped
// rather than aborting the scan.
func parseCodexSession(path string) codexSessionSummary {
	f, err := os.Open(path)
	if err != nil {
		return codexSessionSummary{}
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var s codexSessionSummary
	var lastTS string
	for scanner.Scan() {
		var l codexLine
		if json.Unmarshal(scanner.Bytes(), &l) != nil {
			continue
		}
		switch l.Type {
		case "turn_context":
			var p codexTurnContext
			if json.Unmarshal(l.Payload, &p) == nil && p.Model != "" {
				s.Model = p.Model
			}
		case "event_msg":
			var p codexEventMsg
			if json.Unmarshal(l.Payload, &p) != nil || p.Type != "token_count" {
				continue
			}
			s.Turns++
			u := p.Info.TotalTokenUsage
			s.Tokens = u.TotalTokens
			s.Mix = tokenMix{Input: u.InputTokens, Output: u.OutputTokens,
				CacheRead: u.CachedInputTokens, CacheWrite: u.CacheWriteInputTokens}
			lastTS = l.Timestamp
		}
	}
	if lastTS != "" {
		if ts, parseErr := time.Parse(time.RFC3339, lastTS); parseErr == nil {
			s.Day = ts.Local().Format("2006-01-02")
		}
	}
	return s
}

// codexUsage walks ~/.codex/sessions for *.jsonl files modified within the window, one session per file.
func codexUsage(cutoff time.Time) agentUsage {
	home, err := codexHome()
	if err != nil {
		return agentUsage{Agent: "codex", Note: "store not found: " + err.Error()}
	}
	dir := filepath.Join(home, "sessions")
	if _, statErr := os.Stat(dir); statErr != nil {
		return agentUsage{Agent: "codex", Note: "store not found: " + dir}
	}

	var files []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil || info.ModTime().Before(cutoff) {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if len(files) == 0 {
		return agentUsage{Agent: "codex", Note: "no sessions in window"}
	}

	byModel := map[string]*agentModel{}
	dayCounts := map[string]*dayCount{}
	var total, totalTurns int
	var mix tokenMix
	for _, f := range files {
		s := parseCodexSession(f)
		if s.Model == "" {
			continue
		}
		m := byModel[s.Model]
		if m == nil {
			m = &agentModel{Provider: "openai", Model: s.Model}
			byModel[s.Model] = m
		}
		m.Turns += s.Turns
		m.Tokens += s.Tokens
		total += s.Tokens
		totalTurns += s.Turns
		mix.add(s.Mix)
		if s.Day != "" {
			c := dayCounts[s.Day]
			if c == nil {
				c = &dayCount{}
				dayCounts[s.Day] = c
			}
			c.tokens += s.Tokens
			c.turns += s.Turns
		}
	}
	models := make([]agentModel, 0, len(byModel))
	for _, m := range byModel {
		models = append(models, *m)
	}
	sortModelsByTokens(models)
	daily := dailyFromCounts(dayCounts)
	return agentUsage{Agent: "codex", Sessions: len(files), Turns: totalTurns, Tokens: total, Models: models,
		Mix: mixPtr(mix), Daily: daily, ActiveDays: len(daily)}
}

// lookPathSqlite3 is the "is sqlite3 installed" seam; tests stub it to force the "not installed" note.
var lookPathSqlite3 = func() error {
	_, err := exec.LookPath("sqlite3")
	return err
}

// opencodeSqliteCmd is the opencode.db exec seam; tests replace it so no test shells out to sqlite3.
var opencodeSqliteCmd = func(ctx context.Context, dbPath, query string) ([]byte, error) {
	return exec.CommandContext(ctx, "sqlite3", "-readonly", "-json", dbPath, query).Output()
}

// copilotSqliteCmd is the Copilot CLI session-store.db exec seam; tests replace it likewise.
var copilotSqliteCmd = func(ctx context.Context, dbPath, query string) ([]byte, error) {
	return exec.CommandContext(ctx, "sqlite3", "-readonly", "-json", dbPath, query).Output()
}

// opencodeDBPath honors XDG_DATA_HOME, else ~/.local/share/opencode/opencode.db.
func opencodeDBPath() (string, error) {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "opencode", "opencode.db"), nil
}

type opencodeTotalsRow struct {
	Turns      int     `json:"turns"`
	Tokens     int     `json:"tokens"`
	CostUSD    float64 `json:"cost_usd"`
	Session    int     `json:"sessions"`
	Input      int     `json:"input"`
	Output     int     `json:"output"`
	CacheRead  int     `json:"cache_read"`
	CacheWrite int     `json:"cache_write"`
}
type opencodeModelRow struct {
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	Turns    int     `json:"turns"`
	Tokens   int     `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
}

// opencodeUsage reads opencode's local sqlite store: assistant messages since cutoff, grouped overall
// for Sessions/Turns/Tokens/Cost and separately by providerID/modelID for the Models breakdown.
func opencodeUsage(ctx context.Context, cutoff time.Time) agentUsage {
	dbPath, err := opencodeDBPath()
	if err != nil {
		return agentUsage{Agent: "opencode", Note: "store not found: " + err.Error()}
	}
	if _, statErr := os.Stat(dbPath); statErr != nil {
		return agentUsage{Agent: "opencode", Note: "store not found: " + dbPath}
	}
	if lookErr := lookPathSqlite3(); lookErr != nil {
		return agentUsage{Agent: "opencode", Note: "sqlite3 not installed"}
	}

	cutoffMs := cutoff.UnixMilli()
	where := fmt.Sprintf(`json_extract(data,'$.role')='assistant' AND json_extract(data,'$.time.created') >= %d`, cutoffMs)

	totalsQ := fmt.Sprintf(`SELECT COUNT(*) AS turns, COALESCE(SUM(json_extract(data,'$.tokens.total')),0) AS tokens,
		COALESCE(SUM(json_extract(data,'$.cost')),0) AS cost_usd, COUNT(DISTINCT session_id) AS sessions,
		COALESCE(SUM(json_extract(data,'$.tokens.input')),0) AS input,
		COALESCE(SUM(json_extract(data,'$.tokens.output')),0) AS output,
		COALESCE(SUM(json_extract(data,'$.tokens.cache.read')),0) AS cache_read,
		COALESCE(SUM(json_extract(data,'$.tokens.cache.write')),0) AS cache_write
		FROM message WHERE %s;`, where)
	modelsQ := fmt.Sprintf(`SELECT json_extract(data,'$.providerID') AS provider, json_extract(data,'$.modelID') AS model,
		COUNT(*) AS turns, COALESCE(SUM(json_extract(data,'$.tokens.total')),0) AS tokens,
		COALESCE(SUM(json_extract(data,'$.cost')),0) AS cost_usd
		FROM message WHERE %s GROUP BY provider, model;`, where)
	dailyQ := fmt.Sprintf(`SELECT date(json_extract(data,'$.time.created')/1000,'unixepoch','localtime') AS date,
		COALESCE(SUM(json_extract(data,'$.tokens.total')),0) AS tokens, COUNT(*) AS turns
		FROM message WHERE %s GROUP BY date;`, where)

	totalsOut, totalsErr := opencodeSqliteCmd(ctx, dbPath, totalsQ)
	if totalsErr != nil {
		return agentUsage{Agent: "opencode", Note: "sqlite3 query failed: " + firstLine(totalsOut, totalsErr)}
	}
	var totals []opencodeTotalsRow
	if jsonErr := json.Unmarshal(totalsOut, &totals); jsonErr != nil || len(totals) == 0 || totals[0].Turns == 0 {
		return agentUsage{Agent: "opencode", Note: "no sessions in window"}
	}

	modelsOut, modelsErr := opencodeSqliteCmd(ctx, dbPath, modelsQ)
	if modelsErr != nil {
		return agentUsage{Agent: "opencode", Note: "sqlite3 query failed: " + firstLine(modelsOut, modelsErr)}
	}
	var rows []opencodeModelRow
	_ = json.Unmarshal(modelsOut, &rows)
	models := make([]agentModel, 0, len(rows))
	for _, r := range rows {
		models = append(models, agentModel(r))
	}
	sortModelsByTokens(models)

	daily := fetchDailyBuckets(ctx, dbPath, dailyQ, opencodeSqliteCmd)

	t := totals[0]
	mix := tokenMix{Input: t.Input, Output: t.Output, CacheRead: t.CacheRead, CacheWrite: t.CacheWrite}
	return agentUsage{Agent: "opencode", Sessions: t.Session, Turns: t.Turns, Tokens: t.Tokens, CostUSD: t.CostUSD, Models: models,
		Mix: mixPtr(mix), Daily: daily, ActiveDays: len(daily)}
}

// fetchDailyBuckets runs a GROUP BY date query shaped exactly like []dailyBucket ({date,tokens,turns})
// and returns it sorted ascending; any failure (sqlite3 error, bad JSON) yields an empty, not a panic.
func fetchDailyBuckets(ctx context.Context, dbPath, query string, run func(context.Context, string, string) ([]byte, error)) []dailyBucket {
	out, err := run(ctx, dbPath, query)
	if err != nil {
		return nil
	}
	var daily []dailyBucket
	if json.Unmarshal(out, &daily) != nil {
		return nil
	}
	daily = slices.DeleteFunc(daily, func(d dailyBucket) bool { return d.Date == "" })
	slices.SortFunc(daily, func(a, b dailyBucket) int { return cmp.Compare(a.Date, b.Date) })
	return daily
}

// copilotDBPath honors COPILOT_HOME, else ~/.copilot/session-store.db.
func copilotDBPath() (string, error) {
	base := os.Getenv("COPILOT_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".copilot")
	}
	return filepath.Join(base, "session-store.db"), nil
}

type copilotTotalsRow struct {
	Turns      int `json:"turns"`
	Tokens     int `json:"tokens"`
	Session    int `json:"sessions"`
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cache_read"`
	CacheWrite int `json:"cache_write"`
}
type copilotModelRow struct {
	Model  string `json:"model"`
	Turns  int    `json:"turns"`
	Tokens int    `json:"tokens"`
	NoCost int    `json:"no_cost"`
}

// copilotProvider labels a row: all-NULL-cost databricks- models are BYOK, other costed rows are GitHub-billed.
func copilotProvider(r copilotModelRow) string {
	switch {
	case r.NoCost == r.Turns && strings.HasPrefix(r.Model, models.DatabricksPrefix):
		return providers.GroupDatabricks
	case r.NoCost == 0:
		return "github·copilot"
	}
	return "github-copilot"
}

// copilotTokenSum is the shared input+output+cache-read+cache-write expression, no cost column exists.
const copilotTokenSum = `SUM(COALESCE(input_tokens,0)+COALESCE(output_tokens,0)+COALESCE(cache_read_tokens,0)+COALESCE(cache_write_tokens,0))`

// copilotUsage reads the GitHub Copilot CLI's local sqlite store: assistant_usage_events since cutoff
// (created_at is a 'YYYY-MM-DD HH:MM:SS' UTC string), grouped overall and by model.
func copilotUsage(ctx context.Context, cutoff time.Time) agentUsage {
	dbPath, err := copilotDBPath()
	if err != nil {
		return agentUsage{Agent: "copilot", Note: "store not found: " + err.Error()}
	}
	if _, statErr := os.Stat(dbPath); statErr != nil {
		return agentUsage{Agent: "copilot", Note: "store not found: " + dbPath}
	}
	if lookErr := lookPathSqlite3(); lookErr != nil {
		return agentUsage{Agent: "copilot", Note: "sqlite3 not installed"}
	}

	cutoffStr := cutoff.UTC().Format("2006-01-02 15:04:05")
	where := fmt.Sprintf(`created_at >= '%s'`, cutoffStr)

	totalsQ := fmt.Sprintf(`SELECT COUNT(*) AS turns, %s AS tokens, COUNT(DISTINCT session_id) AS sessions,
		COALESCE(SUM(input_tokens),0) AS input, COALESCE(SUM(output_tokens),0) AS output,
		COALESCE(SUM(cache_read_tokens),0) AS cache_read, COALESCE(SUM(cache_write_tokens),0) AS cache_write
		FROM assistant_usage_events WHERE %s;`, copilotTokenSum, where)
	modelsQ := fmt.Sprintf(`SELECT model, COUNT(*) AS turns, %s AS tokens, SUM(total_nano_aiu IS NULL) AS no_cost
		FROM assistant_usage_events WHERE %s GROUP BY model;`, copilotTokenSum, where)
	dailyQ := fmt.Sprintf(`SELECT date(created_at,'localtime') AS date, %s AS tokens, COUNT(*) AS turns
		FROM assistant_usage_events WHERE %s GROUP BY date;`, copilotTokenSum, where)

	totalsOut, totalsErr := copilotSqliteCmd(ctx, dbPath, totalsQ)
	if totalsErr != nil {
		return agentUsage{Agent: "copilot", Note: "sqlite3 query failed: " + firstLine(totalsOut, totalsErr)}
	}
	var totals []copilotTotalsRow
	if jsonErr := json.Unmarshal(totalsOut, &totals); jsonErr != nil || len(totals) == 0 || totals[0].Turns == 0 {
		return agentUsage{Agent: "copilot", Note: "no sessions in window"}
	}

	modelsOut, modelsErr := copilotSqliteCmd(ctx, dbPath, modelsQ)
	if modelsErr != nil {
		return agentUsage{Agent: "copilot", Note: "sqlite3 query failed: " + firstLine(modelsOut, modelsErr)}
	}
	var rows []copilotModelRow
	_ = json.Unmarshal(modelsOut, &rows)
	byModel := make([]agentModel, 0, len(rows))
	for _, r := range rows {
		byModel = append(byModel, agentModel{Provider: copilotProvider(r), Model: r.Model, Turns: r.Turns, Tokens: r.Tokens})
	}
	sortModelsByTokens(byModel)

	daily := fetchDailyBuckets(ctx, dbPath, dailyQ, copilotSqliteCmd)

	t := totals[0]
	mix := tokenMix{Input: t.Input, Output: t.Output, CacheRead: t.CacheRead, CacheWrite: t.CacheWrite}
	return agentUsage{Agent: "copilot", Sessions: t.Session, Turns: t.Turns, Tokens: t.Tokens, Models: byModel,
		Mix: mixPtr(mix), Daily: daily, ActiveDays: len(daily)}
}

// printAgentsSection renders one total row per agent plus an indented row per model it used.
func printAgentsSection(out io.Writer, agents []agentUsage, days int) {
	fmt.Fprintf(out, "\nAGENTS (last %d days)\n", days)
	fmt.Fprintf(out, "%-10s %-40s %12s %8s %8s %10s\n", "AGENT", "PROVIDER/MODEL", "TOKENS", "TURNS", "SESSIONS", "COST")
	for _, a := range agents {
		if a.Note != "" && len(a.Models) == 0 {
			fmt.Fprintf(out, "%-10s %-40s %s\n", a.Agent, "-", a.Note)
			continue
		}
		fmt.Fprintf(out, "%-10s %-40s %12s %8d %8d %10s\n",
			a.Agent, "(total)", humanTokens(a.Tokens), a.Turns, a.Sessions, costStr(a.CostUSD))
		for _, m := range a.Models {
			fmt.Fprintf(out, "%-10s %-40s %12s %8d %8s %10s\n",
				"", m.Provider+"/"+m.Model, humanTokens(m.Tokens), m.Turns, "-", costStr(m.CostUSD))
		}
	}
}

// costStr renders a dollar amount, "-" when there's nothing to bill (no cost column, or zero).
func costStr(c float64) string {
	if c == 0 {
		return "-"
	}
	return fmt.Sprintf("$%.2f", c)
}
