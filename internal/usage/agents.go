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
	Agent    string       `json:"agent"`
	Sessions int          `json:"sessions"`
	Turns    int          `json:"turns"`
	Tokens   int          `json:"tokens"`
	CostUSD  float64      `json:"cost_usd,omitempty"`
	Models   []agentModel `json:"models,omitempty"`
	Note     string       `json:"note,omitempty"`
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
func buildAgentsUsage(claudeAgg *transcript.UsageAggregate, claudeSessions, days int) []agentUsage {
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

	return []agentUsage{claudeAgentUsage(claudeAgg, claudeSessions), codexU, opencodeU, copilotU}
}

// sortModelsByTokens orders a model slice by tokens descending, in place.
func sortModelsByTokens(models []agentModel) {
	slices.SortFunc(models, func(a, b agentModel) int { return cmp.Compare(b.Tokens, a.Tokens) })
}

// claudeAgentUsage folds the verb's own transcript aggregate (already scoped to the window) into an
// agentUsage row; per-model turn counts aren't tracked at the aggregate level, so Turns stays 0.
func claudeAgentUsage(agg *transcript.UsageAggregate, sessions int) agentUsage {
	if agg == nil || len(agg.Models) == 0 {
		return agentUsage{Agent: "claude", Note: "no sessions in window"}
	}
	var total int
	models := make([]agentModel, 0, len(agg.Models))
	for name, u := range agg.Models {
		tok := int(u.Total())
		total += tok
		models = append(models, agentModel{Provider: "anthropic", Model: name, Tokens: tok})
	}
	sortModelsByTokens(models)
	return agentUsage{Agent: "claude", Sessions: sessions, Tokens: total, Models: models}
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
// carries token_count with the cumulative total_token_usage for the session so far.
type codexLine struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}
type codexTurnContext struct {
	Model string `json:"model"`
}
type codexEventMsg struct {
	Type string `json:"type"`
	Info struct {
		TotalTokenUsage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"total_token_usage"`
	} `json:"info"`
}

// parseCodexSession streams one session jsonl: model is the last turn_context seen, tokens is the last
// token_count's cumulative total (not summed — each line already totals the whole session), turns counts
// the token_count lines. Malformed lines are skipped rather than aborting the scan.
func parseCodexSession(path string) (model string, tokens, turns int) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		var l codexLine
		if json.Unmarshal(scanner.Bytes(), &l) != nil {
			continue
		}
		switch l.Type {
		case "turn_context":
			var p codexTurnContext
			if json.Unmarshal(l.Payload, &p) == nil && p.Model != "" {
				model = p.Model
			}
		case "event_msg":
			var p codexEventMsg
			if json.Unmarshal(l.Payload, &p) != nil || p.Type != "token_count" {
				continue
			}
			turns++
			tokens = p.Info.TotalTokenUsage.TotalTokens
		}
	}
	return model, tokens, turns
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
	var total, totalTurns int
	for _, f := range files {
		model, tokens, turns := parseCodexSession(f)
		if model == "" {
			continue
		}
		m := byModel[model]
		if m == nil {
			m = &agentModel{Provider: "openai", Model: model}
			byModel[model] = m
		}
		m.Turns += turns
		m.Tokens += tokens
		total += tokens
		totalTurns += turns
	}
	models := make([]agentModel, 0, len(byModel))
	for _, m := range byModel {
		models = append(models, *m)
	}
	sortModelsByTokens(models)
	return agentUsage{Agent: "codex", Sessions: len(files), Turns: totalTurns, Tokens: total, Models: models}
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
	Turns   int     `json:"turns"`
	Tokens  int     `json:"tokens"`
	CostUSD float64 `json:"cost_usd"`
	Session int     `json:"sessions"`
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
		COALESCE(SUM(json_extract(data,'$.cost')),0) AS cost_usd, COUNT(DISTINCT session_id) AS sessions
		FROM message WHERE %s;`, where)
	modelsQ := fmt.Sprintf(`SELECT json_extract(data,'$.providerID') AS provider, json_extract(data,'$.modelID') AS model,
		COUNT(*) AS turns, COALESCE(SUM(json_extract(data,'$.tokens.total')),0) AS tokens,
		COALESCE(SUM(json_extract(data,'$.cost')),0) AS cost_usd
		FROM message WHERE %s GROUP BY provider, model;`, where)

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
		models = append(models, agentModel{Provider: r.Provider, Model: r.Model, Turns: r.Turns, Tokens: r.Tokens, CostUSD: r.CostUSD})
	}
	sortModelsByTokens(models)

	t := totals[0]
	return agentUsage{Agent: "opencode", Sessions: t.Session, Turns: t.Turns, Tokens: t.Tokens, CostUSD: t.CostUSD, Models: models}
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
	Turns   int `json:"turns"`
	Tokens  int `json:"tokens"`
	Session int `json:"sessions"`
}
type copilotModelRow struct {
	Model  string `json:"model"`
	Turns  int    `json:"turns"`
	Tokens int    `json:"tokens"`
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

	totalsQ := fmt.Sprintf(`SELECT COUNT(*) AS turns, %s AS tokens, COUNT(DISTINCT session_id) AS sessions
		FROM assistant_usage_events WHERE %s;`, copilotTokenSum, where)
	modelsQ := fmt.Sprintf(`SELECT model, COUNT(*) AS turns, %s AS tokens
		FROM assistant_usage_events WHERE %s GROUP BY model;`, copilotTokenSum, where)

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
	models := make([]agentModel, 0, len(rows))
	for _, r := range rows {
		models = append(models, agentModel{Provider: "github-copilot", Model: r.Model, Turns: r.Turns, Tokens: r.Tokens})
	}
	sortModelsByTokens(models)

	t := totals[0]
	return agentUsage{Agent: "copilot", Sessions: t.Session, Turns: t.Turns, Tokens: t.Tokens, Models: models}
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
