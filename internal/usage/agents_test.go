package usage

// Mirrors antigravity_test.go / claude_test.go's seam-stub style against agents.go's real seams:
// lookPathSqlite3 (the sqlite3-installed check), opencodeSqliteCmd / copilotSqliteCmd (the sqlite3
// -json exec seams, keyed on dbPath+query), and codexUsage's plain filesystem walk under
// CODEX_HOME/sessions (no exec seam — codexHome() honors the env var directly).
import (
	"context"
	"errors"
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

// TestParseCodexSessionFixture: the last token_count wins (cumulative, not summed), turns counts the
// token_count lines (2), the model comes from turn_context, and the malformed line is skipped silently.
func TestParseCodexSessionFixture(t *testing.T) {
	path := filepath.Join("testdata", "codex-session-fixture.jsonl")
	got := parseCodexSession(path)
	if got.Model != "gpt-6-astra" {
		t.Errorf("model = %q, want gpt-6-astra", got.Model)
	}
	if got.Tokens != 2500 {
		t.Errorf("tokens = %d, want 2500 (the LAST token_count, not the sum of 1000+2500)", got.Tokens)
	}
	if got.Turns != 2 {
		t.Errorf("turns = %d, want 2", got.Turns)
	}
	wantMix := tokenMix{Input: 2000, Output: 200, CacheRead: 300}
	if got.Mix != wantMix {
		t.Errorf("mix = %+v, want %+v (the LAST token_count's breakdown)", got.Mix, wantMix)
	}
	if got.Day != "2026-10-23" {
		t.Errorf("day = %q, want 2026-10-23 (the LAST token_count's timestamp, local)", got.Day)
	}
}

// TestCodexUsageParsesFixture: one fixture session file under CODEX_HOME/sessions becomes a codex
// agentUsage row: 1 session, tokens/turns/model from parseCodexSession, provider openai.
func TestCodexUsageParsesFixture(t *testing.T) {
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

	got := codexUsage(time.Now().Add(-24 * time.Hour))
	if got.Agent != "codex" {
		t.Fatalf("agent = %q, want codex", got.Agent)
	}
	if got.Sessions != 1 {
		t.Errorf("sessions = %d, want 1", got.Sessions)
	}
	if got.Turns != 2 {
		t.Errorf("turns = %d, want 2", got.Turns)
	}
	if got.Tokens != 2500 {
		t.Errorf("tokens = %d, want 2500 (last total, not summed)", got.Tokens)
	}
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

	got := codexUsage(time.Now().Add(-24 * time.Hour))
	if got.Note != "no sessions in window" {
		t.Errorf("note = %q, want %q", got.Note, "no sessions in window")
	}
}

// TestCodexUsageMissingStore: no ~/.codex/sessions directory at all reports a "store not found" note.
func TestCodexUsageMissingStore(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	got := codexUsage(time.Now().Add(-24 * time.Hour))
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
	if len(got.Models) != 1 || got.Models[0].Provider != "github-copilot" || got.Models[0].Model != "claude-opus-5.5" {
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

	got := claudeAgentUsage(agg, 3, nil)
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
	got := claudeAgentUsage(nil, 0, nil)
	if got.Note != "no sessions in window" {
		t.Errorf("nil aggregate note = %q, want %q", got.Note, "no sessions in window")
	}
	got = claudeAgentUsage(transcript.NewUsageAggregate(), 0, nil)
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

	got := buildAgentsUsage(transcript.NewUsageAggregate(), 0, 1, nil)
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

// --- claudeSessionCount ---

// TestClaudeSessionCountExplicitSession: an explicit --session with files found counts as 1 session,
// 0 when no files were found for it.
func TestClaudeSessionCountExplicitSession(t *testing.T) {
	if n := claudeSessionCount("", "sess-1", 1, 3); n != 1 {
		t.Errorf("explicit session with files = %d, want 1", n)
	}
	if n := claudeSessionCount("", "sess-1", 1, 0); n != 0 {
		t.Errorf("explicit session with no files = %d, want 0", n)
	}
}

// TestClaudeSessionCountDaysWindow: with no --session, the count is the number of session dirs
// modified within the window.
func TestClaudeSessionCountDaysWindow(t *testing.T) {
	claudeDir := t.TempDir()
	for _, name := range []string{"proj-a", "proj-b"} {
		dir := filepath.Join(claudeDir, "projects", name, "sess-1")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	n := claudeSessionCount(claudeDir, "", 1, 0)
	if n != 2 {
		t.Errorf("session dirs = %d, want 2", n)
	}
}
