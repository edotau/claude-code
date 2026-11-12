// agents.go — per-agent-CLI usage (claude/codex/opencode/copilot) for the "agents" JSON section and the
// AGENTS text table: each reads its own CLI's local store, independent of the transcript-based table above.

package usage

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/table"
)

// agentModel is one provider+profile+model row under an agentUsage.
type agentModel struct {
	Provider string    `json:"provider"`
	Profile  string    `json:"profile,omitempty"`
	Model    string    `json:"model"`
	Turns    int       `json:"turns"`
	Tokens   int       `json:"tokens"`
	CostUSD  float64   `json:"cost_usd,omitempty"`
	Mix      *tokenMix `json:"mix,omitempty"`
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
	out := make([]dailyBucket, 0, len(counts))
	for _, d := range slices.Sorted(maps.Keys(counts)) {
		c := counts[d]
		out = append(out, dailyBucket{Date: d, Tokens: c.tokens, Turns: c.turns})
	}
	return out
}

// claudeSessionCount is the number of sessions the scan covered: one for an explicit --session with any files,
// else the distinct session dirs the in-window transcript files live under.
func claudeSessionCount(session string, files []string) int {
	if session != "" {
		if len(files) > 0 {
			return 1
		}
		return 0
	}
	dirs := map[string]bool{}
	sep := string(filepath.Separator)
	for _, f := range files {
		dir, _, found := strings.Cut(f, sep+"subagents"+sep)
		if !found {
			dir = filepath.Dir(f)
		}
		dirs[dir] = true
	}
	return len(dirs)
}

// buildAgentsUsage runs the codex/opencode/copilot store reads concurrently (10s budget) and folds the
// already-computed claude transcript scan in; always returns all four agents, in this order.
func buildAgentsUsage(scan *transcriptScan, session string, days int) []agentUsage {
	claudeRow := claudeAgentUsage(scan, claudeSessionCount(session, scan.files))
	if session != "" {
		// A claude session id cannot scope the other agents' stores, so they are not read.
		const note = "not scoped to a claude session"
		return []agentUsage{claudeRow, {Agent: "codex", Note: note}, {Agent: "opencode", Note: note}, {Agent: "copilot", Note: note}}
	}
	cutoff := windowCutoff(days)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var codexU, opencodeU, copilotU agentUsage
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); codexU = codexUsage(ctx, cutoff) }()
	go func() { defer wg.Done(); opencodeU = opencodeUsage(ctx, cutoff) }()
	go func() { defer wg.Done(); copilotU = copilotUsage(ctx, cutoff) }()
	wg.Wait()

	return []agentUsage{claudeRow, codexU, opencodeU, copilotU}
}

// sortModelsByTokens orders a model slice by tokens descending, in place.
func sortModelsByTokens(models []agentModel) {
	slices.SortFunc(models, func(a, b agentModel) int { return cmp.Compare(b.Tokens, a.Tokens) })
}

// claudeAgentUsage folds the verb's own transcript scan (already scoped to the window) into an agentUsage
// row; the total's Turns come from the in-window daily buckets, each model's from the aggregate.
func claudeAgentUsage(scan *transcriptScan, sessions int) agentUsage {
	if scan == nil || scan.agg == nil || len(scan.agg.Models) == 0 {
		return agentUsage{Agent: "claude", Note: "no sessions in window"}
	}
	var total, turns int
	for _, d := range scan.daily {
		turns += d.Turns
	}
	var mix tokenMix
	models := make([]agentModel, 0, len(scan.agg.Models))
	for name, u := range scan.agg.Models {
		tok := int(u.Total())
		total += tok
		um := tokenMix{Input: int(u.InputTokens), Output: int(u.OutputTokens),
			CacheRead: int(u.CacheReadTokens), CacheWrite: int(u.CacheCreationTokens)}
		models = append(models, agentModel{Provider: providerOf(name, "anthropic"), Model: name, Turns: scan.agg.ModelTurns[name], Tokens: tok, Mix: mixPtr(um)})
		mix.add(um)
	}
	sortModelsByTokens(models)
	return agentUsage{Agent: "claude", Sessions: sessions, Turns: turns, Tokens: total, Models: models,
		Mix: mixPtr(mix), Daily: scan.daily, ActiveDays: len(scan.daily)}
}

// codexLine mirrors a codex session line: turn_context carries the model, event_msg a cumulative token_count.
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

// codexSessionSummary is one rollout file's in-window share: its model, the token_count lines inside the window,
// the tokens/mix they added, and the same split per local day.
type codexSessionSummary struct {
	Model  string
	Tokens int
	Turns  int
	Mix    tokenMix
	Days   map[string]*dayCount
	err    error // the open or read error that cut the file short; nil when it was read whole
}

// codexCumulative is one token_count line's session-to-date totals, with Mix in disjoint kinds so it sums to Tokens.
type codexCumulative struct {
	Tokens int
	Mix    tokenMix
}

// cumulativeOf maps codex's usage to disjoint kinds: OpenAI's input_tokens already contains cached_input_tokens
// and cache_write_input_tokens, so Input is what remains; any total the parts don't explain stays in Input.
func cumulativeOf(u codexTokenUsage) codexCumulative {
	in := max(u.InputTokens-u.CachedInputTokens-u.CacheWriteInputTokens, 0)
	mix := tokenMix{Input: in, Output: u.OutputTokens, CacheRead: u.CachedInputTokens, CacheWrite: u.CacheWriteInputTokens}
	mix.Input += max(u.TotalTokens-(mix.Input+mix.Output+mix.CacheRead+mix.CacheWrite), 0)
	return codexCumulative{Tokens: mix.Input + mix.Output + mix.CacheRead + mix.CacheWrite, Mix: mix}
}

// minus is the growth from prev to c; a counter that went backwards (a reset) restarts from zero.
func (c codexCumulative) minus(prev codexCumulative) codexCumulative {
	if c.Tokens < prev.Tokens {
		return c
	}
	return codexCumulative{Tokens: c.Tokens - prev.Tokens, Mix: tokenMix{Input: c.Mix.Input - prev.Mix.Input,
		Output: c.Mix.Output - prev.Mix.Output, CacheRead: c.Mix.CacheRead - prev.Mix.CacheRead,
		CacheWrite: c.Mix.CacheWrite - prev.Mix.CacheWrite}}
}

// ctxReader fails its reads once ctx is done, so an expired budget stops the scan mid-file.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// parseCodexSessionCtx streams one session jsonl. Every token_count line carries the session's cumulative totals,
// so a line's own usage is its growth over the previous line; only lines stamped at or after cutoff count (a zero
// cutoff counts all), each dated to its own local day. A line with no RFC3339 stamp counts on fallbackDay. The
// model is the last turn_context seen. Malformed lines are skipped rather than aborting the scan. A cancelled ctx
// stops the read mid-file and reports ctx.Err() in s.err.
func parseCodexSessionCtx(ctx context.Context, path string, cutoff time.Time, fallbackDay string) codexSessionSummary {
	f, err := os.Open(path)
	if err != nil {
		return codexSessionSummary{err: err}
	}
	defer f.Close()
	s := codexSessionSummary{Days: map[string]*dayCount{}}
	var prev codexCumulative
	s.err = textutil.EachLineErr(ctxReader{ctx: ctx, r: f}, func(line []byte) {
		var l codexLine
		if json.Unmarshal(line, &l) != nil {
			return
		}
		switch l.Type {
		case "turn_context":
			var p codexTurnContext
			if json.Unmarshal(l.Payload, &p) == nil && p.Model != "" {
				s.Model = p.Model
			}
		case "event_msg":
			s.addTokenCount(l, &prev, cutoff, fallbackDay)
		}
	})
	return s
}

// addTokenCount folds one event_msg line into s as growth over *prev; other and out-of-window lines add nothing.
func (s *codexSessionSummary) addTokenCount(l codexLine, prev *codexCumulative, cutoff time.Time, fallbackDay string) {
	var p codexEventMsg
	if json.Unmarshal(l.Payload, &p) != nil || p.Type != "token_count" {
		return
	}
	cur := cumulativeOf(p.Info.TotalTokenUsage)
	if cur.Tokens == 0 { // info:null lines carry no totals: no baseline reset, no turn
		return
	}
	delta := cur.minus(*prev)
	*prev = cur
	day := fallbackDay
	if ts, parseErr := time.Parse(time.RFC3339, l.Timestamp); parseErr == nil {
		if ts.Before(cutoff) {
			return
		}
		day = ts.Local().Format("2006-01-02")
	}
	s.Turns++
	s.Tokens += delta.Tokens
	s.Mix.add(delta.Mix)
	if day != "" {
		addDayCount(s.Days, day, delta.Tokens, 1)
	}
}

// addDayCount adds tokens and turns to the day's accumulator, creating it on first use.
func addDayCount(days map[string]*dayCount, day string, tokens, turns int) {
	c := days[day]
	if c == nil {
		c = &dayCount{}
		days[day] = c
	}
	c.tokens += tokens
	c.turns += turns
}

// codexUsage totals in-window token_count growth over ~/.codex/sessions; read errors or ctx expiry mark it incomplete.
func codexUsage(ctx context.Context, cutoff time.Time) agentUsage {
	home, err := paths.CodexHome()
	if err != nil {
		return agentUsage{Agent: "codex", Note: "store not found: " + err.Error()}
	}
	dir := filepath.Join(home, "sessions")
	if _, statErr := os.Stat(dir); statErr != nil {
		return agentUsage{Agent: "codex", Note: "store not found: " + dir}
	}

	files, mtimeDay, errs := codexSessionFiles(ctx, dir, cutoff)
	acc := codexAcc{byModel: map[string]*agentModel{}, days: map[string]*dayCount{}}
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		s := parseCodexSessionCtx(ctx, f, cutoff, mtimeDay[f])
		if s.err != nil {
			errs = append(errs, s.err)
		}
		acc.add(s)
	}
	note := codexIncomplete(errs)
	if acc.sessions == 0 {
		return agentUsage{Agent: "codex", Note: cmp.Or(note, "no sessions in window")}
	}
	models := make([]agentModel, 0, len(acc.byModel))
	for _, m := range acc.byModel {
		models = append(models, *m)
	}
	sortModelsByTokens(models)
	daily := dailyFromCounts(acc.days)
	return agentUsage{Agent: "codex", Sessions: acc.sessions, Turns: acc.turns, Tokens: acc.tokens, Models: models,
		Mix: mixPtr(acc.mix), Daily: daily, ActiveDays: len(daily), Note: note}
}

// codexAcc totals the codex sessions that counted: per model, per day and overall.
type codexAcc struct {
	byModel                 map[string]*agentModel
	days                    map[string]*dayCount
	tokens, turns, sessions int
	mix                     tokenMix
}

// add folds one session in; a session with no model or no in-window turns is not listed.
func (a *codexAcc) add(s codexSessionSummary) {
	if s.Model == "" || s.Turns == 0 {
		return
	}
	addCodexModel(a.byModel, s)
	a.tokens += s.Tokens
	a.turns += s.Turns
	a.mix.add(s.Mix)
	a.sessions++
	for day, d := range s.Days {
		addDayCount(a.days, day, d.tokens, d.turns)
	}
}

// codexIncomplete is the note for a codex total built from partial reads: "" when no read failed.
func codexIncomplete(errs []error) string {
	if len(errs) == 0 {
		return ""
	}
	return fmt.Sprintf("incomplete: %d read error(s), first: %v", len(errs), errs[0])
}

// codexSessionFiles lists *.jsonl under dir modified since cutoff, with each one's mtime day (the fallback day).
// Walk and stat errors come back with the files; a cancelled ctx stops the walk.
func codexSessionFiles(ctx context.Context, dir string, cutoff time.Time) ([]string, map[string]string, []error) {
	var files []string
	var errs []error
	mtimeDay := map[string]string{} // fallback day for a token_count line with no usable timestamp
	werr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			errs = append(errs, walkErr)
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			errs = append(errs, infoErr)
			return nil
		}
		if info.ModTime().Before(cutoff) {
			return nil
		}
		files = append(files, path)
		mtimeDay[path] = info.ModTime().Local().Format("2006-01-02")
		return nil
	})
	if werr != nil {
		errs = append(errs, werr)
	}
	return files, mtimeDay, errs
}

// addCodexModel folds one session into its model's running totals.
func addCodexModel(byModel map[string]*agentModel, s codexSessionSummary) {
	m := byModel[s.Model]
	if m == nil {
		m = &agentModel{Provider: providerOf(s.Model, "openai"), Model: s.Model}
		byModel[s.Model] = m
	}
	m.Turns += s.Turns
	m.Tokens += s.Tokens
	if m.Mix == nil {
		m.Mix = &tokenMix{}
	}
	m.Mix.add(s.Mix)
}

// lookPathSqlite3 is the "is sqlite3 installed" seam; tests stub it to force the "not installed" note.
var lookPathSqlite3 = func() error {
	_, err := exec.LookPath("sqlite3")
	return err
}

func sqliteJSON(ctx context.Context, dbPath, query string) ([]byte, error) {
	return exec.CommandContext(ctx, "sqlite3", "-readonly", "-json", dbPath, query).Output()
}

// sqliteStoreNote is the usage note for a missing store file or missing sqlite3 binary, "" when both are present.
func sqliteStoreNote(dbPath string) string {
	if _, statErr := os.Stat(dbPath); statErr != nil {
		return "store not found: " + dbPath
	}
	if lookErr := lookPathSqlite3(); lookErr != nil {
		return "sqlite3 not installed"
	}
	return ""
}

// sqliteRows runs q through run and decodes its JSON rows; note is the usage note on failure. emptyOK treats the
// zero-row case (sqlite3 -json prints nothing) as no rows instead of unreadable output.
func sqliteRows[T any](ctx context.Context, run func(context.Context, string, string) ([]byte, error), dbPath, q string, emptyOK bool) (rows []T, note string) {
	out, err := run(ctx, dbPath, q)
	if err != nil {
		return nil, "sqlite3 query failed: " + commandErrorLine(out, err)
	}
	if emptyOK && len(bytes.TrimSpace(out)) == 0 {
		return nil, ""
	}
	if jsonErr := json.Unmarshal(out, &rows); jsonErr != nil {
		return nil, "sqlite3 output unreadable: " + jsonErr.Error()
	}
	return rows, ""
}

// opencodeSqliteCmd is the opencode.db exec seam; tests replace it so no test shells out to sqlite3.
var opencodeSqliteCmd = sqliteJSON

// copilotSqliteCmd is the Copilot CLI session-store.db exec seam; tests replace it likewise.
var copilotSqliteCmd = sqliteJSON

// sqliteTotalsRow is the window totals both stores return; copilot has no cost_usd column, so its cost stays 0.
type sqliteTotalsRow struct {
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

// opencodeUsage reads opencode's sqlite store: assistant messages since cutoff, overall and by providerID/modelID.
// tokens.total includes reasoning, so the mix's output does too.
func opencodeUsage(ctx context.Context, cutoff time.Time) agentUsage {
	dbPath := paths.OpencodeDBPath() // honors OPENCODE_DB like opencode's own login probe
	if dbPath == "" {
		return agentUsage{Agent: "opencode", Note: "store not found: OPENCODE_DB is :memory:"}
	}
	if note := sqliteStoreNote(dbPath); note != "" {
		return agentUsage{Agent: "opencode", Note: note}
	}

	totalsQ, modelsQ, dailyQ := opencodeQueries(cutoff.UnixMilli())
	return sqliteAgentUsage(ctx, "opencode", dbPath, opencodeSqliteCmd, totalsQ, modelsQ, dailyQ, func(r opencodeModelRow) agentModel {
		return agentModel{Provider: r.Provider, Model: r.Model, Turns: r.Turns, Tokens: r.Tokens, CostUSD: r.CostUSD}
	})
}

// sqliteAgentUsage is the shared sqlite reader: totals, per-model rows (toModel maps each), then daily buckets.
func sqliteAgentUsage[R any](
	ctx context.Context, agent, dbPath string, run func(context.Context, string, string) ([]byte, error),
	totalsQ, modelsQ, dailyQ string, toModel func(R) agentModel,
) agentUsage {
	totals, note := sqliteRows[sqliteTotalsRow](ctx, run, dbPath, totalsQ, true)
	if note != "" {
		return agentUsage{Agent: agent, Note: note}
	}
	if len(totals) == 0 || totals[0].Turns == 0 {
		return agentUsage{Agent: agent, Note: "no sessions in window"}
	}

	rows, note := sqliteRows[R](ctx, run, dbPath, modelsQ, false)
	if note != "" {
		return agentUsage{Agent: agent, Note: note}
	}
	models := make([]agentModel, 0, len(rows))
	for _, r := range rows {
		models = append(models, toModel(r))
	}
	sortModelsByTokens(models)

	daily, note := fetchDailyBuckets(ctx, dbPath, dailyQ, run)

	t := totals[0]
	mix := tokenMix{Input: t.Input, Output: t.Output, CacheRead: t.CacheRead, CacheWrite: t.CacheWrite}
	return agentUsage{Agent: agent, Sessions: t.Session, Turns: t.Turns, Tokens: t.Tokens, CostUSD: t.CostUSD, Models: models,
		Mix: mixPtr(mix), Daily: daily, ActiveDays: len(daily), Note: note}
}

// opencodeQueries builds the totals, per-model and per-day queries over assistant messages since cutoffMs.
func opencodeQueries(cutoffMs int64) (totalsQ, modelsQ, dailyQ string) {
	where := fmt.Sprintf(`json_extract(data,'$.role')='assistant' AND json_extract(data,'$.time.created') >= %d`, cutoffMs)

	totalsQ = fmt.Sprintf(`SELECT COUNT(*) AS turns, COALESCE(SUM(json_extract(data,'$.tokens.total')),0) AS tokens,
		COALESCE(SUM(json_extract(data,'$.cost')),0) AS cost_usd, COUNT(DISTINCT session_id) AS sessions,
		COALESCE(SUM(json_extract(data,'$.tokens.input')),0) AS input,
		COALESCE(SUM(json_extract(data,'$.tokens.output')),0) + COALESCE(SUM(json_extract(data,'$.tokens.reasoning')),0) AS output,
		COALESCE(SUM(json_extract(data,'$.tokens.cache.read')),0) AS cache_read,
		COALESCE(SUM(json_extract(data,'$.tokens.cache.write')),0) AS cache_write
		FROM message WHERE %s;`, where)
	modelsQ = fmt.Sprintf(`SELECT json_extract(data,'$.providerID') AS provider, json_extract(data,'$.modelID') AS model,
		COUNT(*) AS turns, COALESCE(SUM(json_extract(data,'$.tokens.total')),0) AS tokens,
		COALESCE(SUM(json_extract(data,'$.cost')),0) AS cost_usd
		FROM message WHERE %s GROUP BY provider, model;`, where)
	dailyQ = fmt.Sprintf(`SELECT date(json_extract(data,'$.time.created')/1000,'unixepoch','localtime') AS date,
		COALESCE(SUM(json_extract(data,'$.tokens.total')),0) AS tokens, COUNT(*) AS turns
		FROM message WHERE %s GROUP BY date;`, where)
	return totalsQ, modelsQ, dailyQ
}

// fetchDailyBuckets runs a GROUP BY date query into ascending []dailyBucket; a failure returns a note instead.
func fetchDailyBuckets(ctx context.Context, dbPath, query string, run func(context.Context, string, string) ([]byte, error)) ([]dailyBucket, string) {
	daily, note := sqliteRows[dailyBucket](ctx, run, dbPath, query, true)
	if note != "" {
		return nil, "daily series: " + note
	}
	daily = slices.DeleteFunc(daily, func(d dailyBucket) bool { return d.Date == "" })
	slices.SortFunc(daily, func(a, b dailyBucket) int { return cmp.Compare(a.Date, b.Date) })
	return daily, ""
}

// copilotDBPath honors COPILOT_HOME, else ~/.copilot/session-store.db.
func copilotDBPath() (string, error) {
	base, err := paths.CopilotHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "session-store.db"), nil
}

type copilotModelRow struct {
	Model  string `json:"model"`
	Turns  int    `json:"turns"`
	Tokens int    `json:"tokens"`
	NoCost int    `json:"no_cost"`
}

// providerOf labels a model row: databricks- endpoint ids are Databricks-routed, else the agent's own provider def.
func providerOf(model, def string) string {
	if strings.HasPrefix(model, models.DatabricksPrefix) {
		return providers.GroupDatabricks
	}
	return def
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
// (created_at is ISO 'YYYY-MM-DDTHH:MM:SS.sssZ', so datetime() normalizes it), grouped overall and by model.
func copilotUsage(ctx context.Context, cutoff time.Time) agentUsage {
	dbPath, err := copilotDBPath()
	if err != nil {
		return agentUsage{Agent: "copilot", Note: "store not found: " + err.Error()}
	}
	if note := sqliteStoreNote(dbPath); note != "" {
		return agentUsage{Agent: "copilot", Note: note}
	}

	cutoffStr := cutoff.UTC().Format("2006-01-02 15:04:05")
	where := fmt.Sprintf(`datetime(created_at) >= '%s'`, cutoffStr)

	totalsQ := fmt.Sprintf(`SELECT COUNT(*) AS turns, %s AS tokens, COUNT(DISTINCT session_id) AS sessions,
		COALESCE(SUM(input_tokens),0) AS input, COALESCE(SUM(output_tokens),0) AS output,
		COALESCE(SUM(cache_read_tokens),0) AS cache_read, COALESCE(SUM(cache_write_tokens),0) AS cache_write
		FROM assistant_usage_events WHERE %s;`, copilotTokenSum, where)
	modelsQ := fmt.Sprintf(`SELECT model, COUNT(*) AS turns, %s AS tokens, SUM(total_nano_aiu IS NULL) AS no_cost
		FROM assistant_usage_events WHERE %s GROUP BY model;`, copilotTokenSum, where)
	dailyQ := fmt.Sprintf(`SELECT date(created_at,'localtime') AS date, %s AS tokens, COUNT(*) AS turns
		FROM assistant_usage_events WHERE %s GROUP BY date;`, copilotTokenSum, where)

	return sqliteAgentUsage(ctx, "copilot", dbPath, copilotSqliteCmd, totalsQ, modelsQ, dailyQ, func(r copilotModelRow) agentModel {
		prov, profile, _ := strings.Cut(copilotProvider(r), "·")
		return agentModel{Provider: prov, Profile: profile, Model: r.Model, Turns: r.Turns, Tokens: r.Tokens}
	})
}

// printAgentsSection renders one total row per agent plus an indented row per model it used.
func printAgentsSection(out io.Writer, agents []agentUsage, session string, days int) {
	window := "session " + session
	switch {
	case session != "":
	case days == 1:
		window = "last 1 day"
	default:
		window = fmt.Sprintf("last %d days", days)
	}
	fmt.Fprintf(out, "\nAGENTS (%s)\n", window)
	r := func(w int, s string) string { return fmt.Sprintf("%*s", w, s) }
	head := []string{"AGENT", "PROVIDER/MODEL", r(12, "TOKENS"), r(8, "TURNS"), r(8, "SESSIONS"), r(10, "COST")}
	var rows [][]string
	for _, a := range agents {
		if a.Note != "" && len(a.Models) == 0 {
			rows = append(rows, []string{a.Agent, "-", a.Note})
			continue
		}
		rows = append(rows, []string{a.Agent, "(total)", r(12, humanTokens(a.Tokens)), r(8, strconv.Itoa(a.Turns)),
			r(8, strconv.Itoa(a.Sessions)), r(10, costStr(a.CostUSD))})
		for _, m := range a.Models {
			rows = append(rows, []string{"", rowLabel(m.Provider, m.Profile) + "/" + m.Model, r(12, humanTokens(m.Tokens)),
				r(8, strconv.Itoa(m.Turns)), r(8, "-"), r(10, costStr(m.CostUSD))})
		}
		if a.Note != "" { // a total built from partial reads says so under its own row
			rows = append(rows, []string{"", a.Note})
		}
	}
	fmt.Fprint(out, table.Render(head, nil, rows, 0))
}

// costStr renders a dollar amount, "-" when there's nothing to bill (no cost column, or zero).
func costStr(c float64) string {
	if c == 0 {
		return "-"
	}
	return fmt.Sprintf("$%.2f", c)
}
