package usage

// Mirrors antigravity_test.go / claude_test.go's seam-stub style against agents.go's real seams:
// lookPathSqlite3 (the sqlite3-installed check), opencodeSqliteCmd / copilotSqliteCmd (the sqlite3
// -json exec seams, keyed on dbPath+query), and codexUsage's plain filesystem walk under
// CODEX_HOME/sessions (no exec seam — paths.CodexHome() honors the env var directly).
import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/transcript"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// stubLookPathSqlite3 replaces the "is sqlite3 installed" seam for the duration of the test.
func stubLookPathSqlite3(t *testing.T, fn func() error) {
	t.Helper()
	orig := lookPathSqlite3
	lookPathSqlite3 = fn
	t.Cleanup(func() { lookPathSqlite3 = orig })
}

// stubOpencodeSqlite replaces opencodeSqliteCmd, routing a GROUP BY query to the models fixture and
// everything else to the totals fixture.
func stubOpencodeSqlite(t *testing.T, totals, models []byte) {
	t.Helper()
	orig := opencodeSqliteCmd
	opencodeSqliteCmd = func(_ context.Context, _ string, query string) ([]byte, error) {
		if strings.Contains(query, "GROUP BY") {
			return models, nil
		}
		return totals, nil
	}
	t.Cleanup(func() { opencodeSqliteCmd = orig })
}

// stubCopilotSqlite mirrors stubOpencodeSqlite for copilotSqliteCmd.
func stubCopilotSqlite(t *testing.T, totals, models []byte) {
	t.Helper()
	orig := copilotSqliteCmd
	copilotSqliteCmd = func(_ context.Context, _ string, query string) ([]byte, error) {
		if strings.Contains(query, "GROUP BY") {
			return models, nil
		}
		return totals, nil
	}
	t.Cleanup(func() { copilotSqliteCmd = orig })
}

// writeOpencodeStore creates an empty file at the opencode DB path so the store's existence check
// passes (opencodeUsage only stats the path; the real sqlite3 call is stubbed).
func writeOpencodeStore(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	full := filepath.Join(dir, "opencode")
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, "opencode.db"), []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeCopilotStore mirrors writeOpencodeStore for the Copilot CLI's session-store.db.
func writeCopilotStore(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("COPILOT_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "session-store.db"), []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- codex: parseCodexSession + codexUsage ---

// TestParseCodexSessionFixture: token_count lines are cumulative, so each line adds only its growth (850 then
// 1350); turns counts the lines, the model comes from turn_context, the malformed line is skipped silently, and
// the mix splits input_tokens (which already contains cached_input_tokens) into disjoint kinds.
func TestParseCodexSessionFixture(t *testing.T) {
	path := filepath.Join("testdata", "codex-session-fixture.jsonl")
	got := parseCodexSession(path, time.Time{}, "")
	if got.Model != "gpt-6-astra" {
		t.Errorf("model = %q, want gpt-6-astra", got.Model)
	}
	if got.Tokens != 2200 || got.Turns != 2 {
		t.Errorf("tokens/turns = %d/%d, want 2200/2 (growth summed, not the last total twice)", got.Tokens, got.Turns)
	}
	wantMix := tokenMix{Input: 1700, Output: 200, CacheRead: 300}
	if got.Mix != wantMix {
		t.Errorf("mix = %+v, want %+v (cached tokens are not input as well)", got.Mix, wantMix)
	}
	dayTokens := 0
	for _, d := range got.Days {
		dayTokens += d.tokens
	}
	if len(got.Days) < 1 || dayTokens != 2200 {
		t.Errorf("days = %+v, want the same 2200 tokens spread over the lines' own days", got.Days)
	}
}

// TestParseCodexSessionCountsOnlyGrowthInsideWindow: with a cutoff between the two token_count lines only the
// second line's growth (1350 = 2200-850) counts, and its mix is the delta, still summing to its tokens.
func TestParseCodexSessionCountsOnlyGrowthInsideWindow(t *testing.T) {
	cutoff := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	got := parseCodexSession(filepath.Join("testdata", "codex-session-fixture.jsonl"), cutoff, "")
	if got.Tokens != 1350 || got.Turns != 1 {
		t.Fatalf("tokens/turns = %d/%d, want 1350/1 (the earlier 850 predates the window)", got.Tokens, got.Turns)
	}
	if want := (tokenMix{Input: 1050, Output: 150, CacheRead: 150}); got.Mix != want {
		t.Errorf("mix = %+v, want %+v", got.Mix, want)
	}
}

// codexFixtureUsage writes the fixture as the only session under a temp CODEX_HOME and returns codexUsage over it.
func codexFixtureUsage(t *testing.T) agentUsage {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	sessDir := filepath.Join(home, "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := readFixture(t, "codex-session-fixture.jsonl")
	if err := os.WriteFile(filepath.Join(sessDir, "rollout.jsonl"), fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	return codexUsage(context.Background(), time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC))
}

// TestCodexUsageParsesFixture: one fixture session file under CODEX_HOME/sessions becomes a codex
// agentUsage row: 1 session, tokens/turns/model from parseCodexSession, provider openai.
func TestCodexUsageParsesFixture(t *testing.T) {
	got := codexFixtureUsage(t)
	if got.Agent != "codex" {
		t.Fatalf("agent = %q, want codex", got.Agent)
	}
	if got.Sessions != 1 {
		t.Errorf("sessions = %d, want 1", got.Sessions)
	}
	if got.Turns != 2 {
		t.Errorf("turns = %d, want 2", got.Turns)
	}
	if got.Tokens != 2200 {
		t.Errorf("tokens = %d, want 2200", got.Tokens)
	}
}

// TestCodexUsageFixtureModelRow: the fixture's model row is openai/gpt-6-astra and the row carries no note.
func TestCodexUsageFixtureModelRow(t *testing.T) {
	got := codexFixtureUsage(t)
	if len(got.Models) != 1 || got.Models[0].Provider != "openai" || got.Models[0].Model != "gpt-6-astra" {
		t.Errorf("models = %+v, want one openai/gpt-6-astra row", got.Models)
	}
	if got.Note != "" {
		t.Errorf("unexpected note %q on a successful row", got.Note)
	}
}

// TestCodexUsageCutoffExcludesOldSessions: a session file whose mtime is older than the window is
// excluded, leaving no sessions in window.
func TestCodexUsageCutoffExcludesOldSessions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	sessDir := filepath.Join(home, "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sessDir, "old-rollout.jsonl")
	if err := os.WriteFile(path, readFixture(t, "codex-session-fixture.jsonl"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	got := codexUsage(context.Background(), time.Now().Add(-24*time.Hour))
	if got.Note != "no sessions in window" {
		t.Errorf("note = %q, want %q", got.Note, "no sessions in window")
	}
}

// TestCodexUsageMissingStore: no ~/.codex/sessions directory at all reports a "store not found" note.
func TestCodexUsageMissingStore(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	got := codexUsage(context.Background(), time.Now().Add(-24*time.Hour))
	if !strings.HasPrefix(got.Note, "store not found") {
		t.Errorf("note = %q, want a store-not-found note", got.Note)
	}
}

// --- opencode: opencodeUsage ---

// TestOpencodeUsageParsesFixture: the sqlite3 -json fixtures become one agentUsage row, cost summed
// and sessions the distinct count the totals query reports.
func TestOpencodeUsageParsesFixture(t *testing.T) {
	writeOpencodeStore(t)
	stubLookPathSqlite3(t, func() error { return nil })
	stubOpencodeSqlite(t,
		readFixture(t, "opencode-totals-fixture.json"),
		readFixture(t, "opencode-models-fixture.json"))

	got := opencodeUsage(context.Background(), time.Now().Add(-24*time.Hour))
	if got.Agent != "opencode" {
		t.Fatalf("agent = %q, want opencode", got.Agent)
	}
	if got.Sessions != 6 {
		t.Errorf("sessions = %d, want 6 (distinct session_id count)", got.Sessions)
	}
	if got.Turns != 97 {
		t.Errorf("turns = %d, want 97", got.Turns)
	}
	if got.Tokens != 320866 {
		t.Errorf("tokens = %d, want 320866", got.Tokens)
	}
	if got.CostUSD != 0.08 {
		t.Errorf("cost = %v, want 0.08 (summed cost column)", got.CostUSD)
	}
	if len(got.Models) != 1 || got.Models[0].Provider != "github-copilot" || got.Models[0].Model != "mai-code-1.1-flash" {
		t.Errorf("models = %+v", got.Models)
	}
	if got.Note != "" {
		t.Errorf("unexpected note %q on a successful row", got.Note)
	}
}

// TestOpencodeUsageMissingStore: no opencode.db at all reports a "store not found" note, never calling sqlite3.
func TestOpencodeUsageMissingStore(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	stubLookPathSqlite3(t, func() error { return nil })
	stubOpencodeSqlite(t, nil, nil) // would be an empty, falsely-"successful" parse if ever called
	got := opencodeUsage(context.Background(), time.Now().Add(-24*time.Hour))
	if !strings.HasPrefix(got.Note, "store not found") {
		t.Errorf("note = %q, want a store-not-found note", got.Note)
	}
}

// An absolute OPENCODE_DB is the store usage reads, as opencode's own login probe does; XDG's default is ignored.
func TestOpencodeUsageHonorsOpencodeDB(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir()) // the default store does not exist
	db := filepath.Join(t.TempDir(), "opencode-beta.db")
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE_DB", db)
	stubLookPathSqlite3(t, func() error { return nil })
	var read string
	orig := opencodeSqliteCmd
	opencodeSqliteCmd = func(_ context.Context, path, _ string) ([]byte, error) { read = path; return []byte("[]"), nil }
	t.Cleanup(func() { opencodeSqliteCmd = orig })
	got := opencodeUsage(context.Background(), time.Now().Add(-24*time.Hour))
	if read != db {
		t.Errorf("sqlite read %q, want the OPENCODE_DB store %q (note %q)", read, db, got.Note)
	}
}

// TestOpencodeUsageSqlite3Missing: the store exists but sqlite3 isn't installed reports "sqlite3 not installed".
func TestOpencodeUsageSqlite3Missing(t *testing.T) {
	writeOpencodeStore(t)
	stubLookPathSqlite3(t, func() error { return errors.New("exec: \"sqlite3\": executable file not found in $PATH") })
	got := opencodeUsage(context.Background(), time.Now().Add(-24*time.Hour))
	if got.Note != "sqlite3 not installed" {
		t.Errorf("note = %q, want %q", got.Note, "sqlite3 not installed")
	}
}

// --- copilot: copilotUsage ---

// TestCopilotUsageParsesFixture: tokens is the sum of the four columns the totals query already added,
// sessions the distinct count.
func TestCopilotUsageParsesFixture(t *testing.T) {
	writeCopilotStore(t)
	stubLookPathSqlite3(t, func() error { return nil })
	stubCopilotSqlite(t,
		readFixture(t, "copilot-totals-fixture.json"),
		readFixture(t, "copilot-models-fixture.json"))

	got := copilotUsage(context.Background(), time.Now().Add(-24*time.Hour))
	if got.Agent != "copilot" {
		t.Fatalf("agent = %q, want copilot", got.Agent)
	}
	if got.Sessions != 2 {
		t.Errorf("sessions = %d, want 2", got.Sessions)
	}
	if got.Turns != 2 {
		t.Errorf("turns = %d, want 2", got.Turns)
	}
	if got.Tokens != 1701190 {
		t.Errorf("tokens = %d, want 1701190 (input+output+cache_read+cache_write)", got.Tokens)
	}
	if len(got.Models) != 1 || got.Models[0].Provider != "github" || got.Models[0].Profile != "copilot" || got.Models[0].Model != "claude-opus-5.5" {
		t.Errorf("models = %+v", got.Models)
	}
	if got.Note != "" {
		t.Errorf("unexpected note %q on a successful row", got.Note)
	}
}

// TestCopilotUsageMissingStore: no session-store.db at all reports a "store not found" note.
func TestCopilotUsageMissingStore(t *testing.T) {
	t.Setenv("COPILOT_HOME", t.TempDir())
	stubLookPathSqlite3(t, func() error { return nil })
	got := copilotUsage(context.Background(), time.Now().Add(-24*time.Hour))
	if !strings.HasPrefix(got.Note, "store not found") {
		t.Errorf("note = %q, want a store-not-found note", got.Note)
	}
}

// TestCopilotUsageSqlite3Missing: the store exists but sqlite3 isn't installed reports "sqlite3 not installed".
func TestCopilotUsageSqlite3Missing(t *testing.T) {
	writeCopilotStore(t)
	stubLookPathSqlite3(t, func() error { return errors.New("not found") })
	got := copilotUsage(context.Background(), time.Now().Add(-24*time.Hour))
	if got.Note != "sqlite3 not installed" {
		t.Errorf("note = %q, want %q", got.Note, "sqlite3 not installed")
	}
}

// --- sortModelsByTokens ---

// TestSortModelsByTokens: models sort tokens descending, in place.
func TestSortModelsByTokens(t *testing.T) {
	models := []agentModel{
		{Model: "low", Tokens: 10},
		{Model: "high", Tokens: 100},
		{Model: "mid", Tokens: 50},
	}
	sortModelsByTokens(models)
	got := []string{models[0].Model, models[1].Model, models[2].Model}
	want := []string{"high", "mid", "low"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("order = %v, want %v", got, want)
			break
		}
	}
}

// --- claudeAgentUsage ---

// TestClaudeAgentUsageFoldsAggregate: a transcript.UsageAggregate with two models folds into one
// agentUsage row, tokens summed and the model list sorted tokens descending.
func TestClaudeAgentUsageFoldsAggregate(t *testing.T) {
	agg := transcript.NewUsageAggregate()
	agg.Models["sonnet"] = &transcript.ModelUsage{InputTokens: 100}
	agg.Models["haiku"] = &transcript.ModelUsage{InputTokens: 900}

	got := claudeAgentUsage(&transcriptScan{agg: agg}, 3)
	if got.Agent != "claude" {
		t.Fatalf("agent = %q, want claude", got.Agent)
	}
	if got.Sessions != 3 {
		t.Errorf("sessions = %d, want 3", got.Sessions)
	}
	if got.Tokens != 1000 {
		t.Errorf("tokens = %d, want 1000", got.Tokens)
	}
	if len(got.Models) != 2 || got.Models[0].Model != "haiku" {
		t.Errorf("models = %+v, want haiku first (900 > 100)", got.Models)
	}
}

// TestClaudeAgentUsageEmptyAggregate: nil or model-less aggregate reports "no sessions in window".
func TestClaudeAgentUsageEmptyAggregate(t *testing.T) {
	got := claudeAgentUsage(nil, 0)
	if got.Note != "no sessions in window" {
		t.Errorf("nil aggregate note = %q, want %q", got.Note, "no sessions in window")
	}
	got = claudeAgentUsage(&transcriptScan{agg: transcript.NewUsageAggregate()}, 0)
	if got.Note != "no sessions in window" {
		t.Errorf("empty aggregate note = %q, want %q", got.Note, "no sessions in window")
	}
}

// --- buildAgentsUsage: the composed entry point ---

// TestBuildAgentsUsageAlwaysReturnsFour: all four agents are always present, in order
// claude/codex/opencode/copilot, even when every store is missing.
func TestBuildAgentsUsageAlwaysReturnsFour(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("COPILOT_HOME", t.TempDir())
	stubLookPathSqlite3(t, func() error { return nil })

	got := buildAgentsUsage(&transcriptScan{agg: transcript.NewUsageAggregate()}, "", 1)
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4", len(got))
	}
	want := []string{"claude", "codex", "opencode", "copilot"}
	for i, w := range want {
		if got[i].Agent != w {
			t.Errorf("agent[%d] = %q, want %q", i, got[i].Agent, w)
		}
	}
}

// TestBuildAgentsUsageSessionScopesOnlyClaude: with a session, the other agents are skipped with a not-scoped note.
func TestBuildAgentsUsageSessionScopesOnlyClaude(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("COPILOT_HOME", t.TempDir())
	stubLookPathSqlite3(t, func() error { return nil })

	got := buildAgentsUsage(&transcriptScan{agg: transcript.NewUsageAggregate()}, "sess-1", 0)
	if len(got) != 4 || got[0].Agent != "claude" {
		t.Fatalf("got %+v, want claude first of four rows", got)
	}
	for _, row := range got[1:] {
		if row.Note != "not scoped to a claude session" {
			t.Errorf("%s note = %q, want the not-scoped note", row.Agent, row.Note)
		}
	}
}

// --- claudeSessionCount ---

// TestClaudeSessionCountExplicitSession: an explicit --session with files found counts as 1 session,
// 0 when no files were found for it.
func TestClaudeSessionCountExplicitSession(t *testing.T) {
	if n := claudeSessionCount("sess-1", []string{"a.jsonl", "b.jsonl"}); n != 1 {
		t.Errorf("explicit session with files = %d, want 1", n)
	}
	if n := claudeSessionCount("sess-1", nil); n != 0 {
		t.Errorf("explicit session with no files = %d, want 0", n)
	}
}

// TestClaudeSessionCountDaysWindow: with no --session, the count is the distinct session dirs the in-window
// files live under (workflow agents share their session's dir).
func TestClaudeSessionCountDaysWindow(t *testing.T) {
	sep := string(filepath.Separator)
	p := func(parts ...string) string { return strings.Join(parts, sep) }
	files := []string{
		p("c", "projects", "proj-a", "sess-1", "subagents", "agent-a.jsonl"),
		p("c", "projects", "proj-a", "sess-1", "subagents", "workflows", "wf_1", "agent-b.jsonl"),
		p("c", "projects", "proj-b", "sess-1", "subagents", "agent-a.jsonl"),
	}
	if n := claudeSessionCount("", files); n != 2 {
		t.Errorf("session dirs = %d, want 2", n)
	}
}

// TestCopilotProviderLabel pins BYOK (all-NULL-cost databricks- rows) vs GitHub-billed labelling.
func TestCopilotProviderLabel(t *testing.T) {
	tests := []struct {
		name string
		row  copilotModelRow
		want string
	}{
		{"byok databricks all NULL cost", copilotModelRow{Model: "databricks-claude-opus-5-5", Turns: 3, NoCost: 3}, "databricks"},
		{"databricks name but costed turns", copilotModelRow{Model: "databricks-claude-opus-5-5", Turns: 3, NoCost: 0}, "github·copilot"},
		{"databricks name mixed cost", copilotModelRow{Model: "databricks-claude-opus-5-5", Turns: 3, NoCost: 1}, "github-copilot"},
		{"github model all costed", copilotModelRow{Model: "claude-opus-5.5", Turns: 2, NoCost: 0}, "github·copilot"},
		{"github model all NULL cost", copilotModelRow{Model: "claude-opus-5.5", Turns: 2, NoCost: 2}, "github-copilot"},
		{"github model mixed", copilotModelRow{Model: "claude-opus-5.5", Turns: 2, NoCost: 1}, "github-copilot"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := copilotProvider(tt.row); got != tt.want {
				t.Errorf("copilotProvider(%+v) = %q, want %q", tt.row, got, tt.want)
			}
		})
	}
}

// TestCopilotUsageLabelsBYOKRows pins the label end to end through the sqlite stub (BYOK, mixed-cost, costed rows).
func TestCopilotUsageLabelsBYOKRows(t *testing.T) {
	writeCopilotStore(t)
	stubLookPathSqlite3(t, func() error { return nil })
	stubCopilotSqlite(t,
		[]byte(`[{"sessions":1,"turns":6,"input":1,"output":1,"cache_read":0,"cache_write":0,"tokens":10}]`),
		[]byte(`[{"model":"databricks-claude-opus-5-5","turns":3,"tokens":5,"no_cost":3},`+
			`{"model":"claude-opus-5.5","turns":2,"tokens":3,"no_cost":0},`+
			`{"model":"gpt-5","turns":1,"tokens":2,"no_cost":1}]`))
	got := copilotUsage(context.Background(), time.Now().Add(-24*time.Hour))
	want := map[string]string{"databricks-claude-opus-5-5": "databricks", "claude-opus-5.5": "github·copilot", "gpt-5": "github-copilot"}
	if len(got.Models) != 3 {
		t.Fatalf("models = %+v", got.Models)
	}
	for _, m := range got.Models {
		if rowLabel(m.Provider, m.Profile) != want[m.Model] {
			t.Errorf("%s labelled %q, want %q", m.Model, rowLabel(m.Provider, m.Profile), want[m.Model])
		}
	}
}

// mixSum is the four token kinds of a mix added up.
func mixSum(m *tokenMix) int {
	if m == nil {
		return 0
	}
	return m.Input + m.Output + m.CacheRead + m.CacheWrite
}

// TestAgentMixSumsToTokens: for every agent the four mix kinds add up to the agent's tokens, i.e. no kind is
// counted twice (codex input contains its cached tokens) or dropped (opencode's reasoning tokens).
func TestAgentMixSumsToTokens(t *testing.T) {
	claudeDir := t.TempDir()
	writeUsageFixture(t, claudeDir, "-p", "s", time.Hour, usageRecordLine("claude-sonnet-5", 100, 10, recentTS(time.Hour)))
	scan, err := scanUsage(claudeDir, "", 1, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "sessions", "r.jsonl"), readFixture(t, "codex-session-fixture.jsonl"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeOpencodeStore(t)
	writeCopilotStore(t)
	stubLookPathSqlite3(t, func() error { return nil })
	stubCopilotSqlite(t, readFixture(t, "copilot-totals-fixture.json"), readFixture(t, "copilot-models-fixture.json"))
	stubOpencodeSqlite(t, readFixture(t, "opencode-totals-fixture.json"), readFixture(t, "opencode-models-fixture.json"))

	agents := map[string]agentUsage{
		"claude":   claudeAgentUsage(scan, 1),
		"codex":    codexUsage(context.Background(), time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)),
		"opencode": opencodeUsage(context.Background(), time.Now().Add(-24*time.Hour)),
		"copilot":  copilotUsage(context.Background(), time.Now().Add(-24*time.Hour)),
	}
	wantTokens := map[string]int{"claude": 110, "codex": 2200, "opencode": 320866, "copilot": 1701190}
	for name, a := range agents {
		if a.Tokens != wantTokens[name] {
			t.Errorf("%s: tokens = %d, want %d (note %q)", name, a.Tokens, wantTokens[name], a.Note)
		}
		if got := mixSum(a.Mix); got != a.Tokens {
			t.Errorf("%s: sum(mix) = %d, tokens = %d (mix %+v)", name, got, a.Tokens, a.Mix)
		}
		for _, m := range a.Models {
			if m.Mix != nil && mixSum(m.Mix) != m.Tokens {
				t.Errorf("%s/%s: sum(mix) = %d, tokens = %d", name, m.Model, mixSum(m.Mix), m.Tokens)
			}
		}
	}
}

// TestOpencodeTotalsQueryFoldsReasoningIntoOutput: opencode's tokens.total includes reasoning tokens, so the mix
// query must add them to output or sum(mix) falls short of tokens (the real sqlite3 is not on the test PATH).
func TestOpencodeTotalsQueryFoldsReasoningIntoOutput(t *testing.T) {
	writeOpencodeStore(t)
	stubLookPathSqlite3(t, func() error { return nil })
	var totalsQuery string
	orig := opencodeSqliteCmd
	opencodeSqliteCmd = func(_ context.Context, _, query string) ([]byte, error) {
		if !strings.Contains(query, "GROUP BY") && totalsQuery == "" {
			totalsQuery = query
		}
		return []byte(`[]`), nil
	}
	t.Cleanup(func() { opencodeSqliteCmd = orig })
	opencodeUsage(context.Background(), time.Now().Add(-24*time.Hour))
	if !strings.Contains(totalsQuery, "$.tokens.reasoning") {
		t.Errorf("totals query does not sum tokens.reasoning into output:\n%s", totalsQuery)
	}
}
