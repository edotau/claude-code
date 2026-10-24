package usage

// TEST mode (agents/copilot.md): pinned to the Mix/Daily/ActiveDays contract on agentUsage and the
// top-level window_days field. codex and claude are fully landed (agents.go); opencode and copilot
// declare the row types (opencodeMixRow/opencodeDailyRow) but never query or fold them in — those tests
// below fail against REAL code and are reported as incomplete, not patched.
//
// Fixture-shape assumptions, flagged for a follow-up alignment pass once opencode/copilot land:
//   - opencode daily rows use the "day" key, matching the already-declared (but unused) opencodeDailyRow.
//   - opencode/copilot mix is assumed foldable from the SAME totals row (extra input/output/cache_read/
//     cache_write columns); copilot has no declared row type at all, so this is a guess.
//   - the daily query is routed in the stub by the query containing "date(" — unconfirmed for either agent.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/transcript"
)

// stubOpencodeSqliteWithDaily extends stubOpencodeSqlite with a third fixture for a per-day query.
func stubOpencodeSqliteWithDaily(t *testing.T, totals, models, daily []byte) {
	t.Helper()
	orig := opencodeSqliteCmd
	opencodeSqliteCmd = func(_ context.Context, _ string, query string) ([]byte, error) {
		switch {
		case strings.Contains(query, "date("):
			return daily, nil
		case strings.Contains(query, "GROUP BY"):
			return models, nil
		default:
			return totals, nil
		}
	}
	t.Cleanup(func() { opencodeSqliteCmd = orig })
}

// stubCopilotSqliteWithDaily mirrors stubOpencodeSqliteWithDaily for copilotSqliteCmd.
func stubCopilotSqliteWithDaily(t *testing.T, totals, models, daily []byte) {
	t.Helper()
	orig := copilotSqliteCmd
	copilotSqliteCmd = func(_ context.Context, _ string, query string) ([]byte, error) {
		switch {
		case strings.Contains(query, "date("):
			return daily, nil
		case strings.Contains(query, "GROUP BY"):
			return models, nil
		default:
			return totals, nil
		}
	}
	t.Cleanup(func() { copilotSqliteCmd = orig })
}

// requireMix fails fast with a clear message instead of a nil-pointer panic when Mix wasn't populated.
func requireMix(t *testing.T, a agentUsage) tokenMix {
	t.Helper()
	if a.Mix == nil {
		t.Fatalf("%s: Mix is nil, want a populated mix", a.Agent)
	}
	return *a.Mix
}

// --- codex: Mix + Daily + ActiveDays from the extended fixture (landed) ---

// TestCodexUsageMix: the LAST token_count's input/cached_input/output map to Mix.Input/CacheRead/Output;
// codex's cache_write_input_tokens is absent from the fixture, so Mix.CacheWrite stays 0.
func TestCodexUsageMix(t *testing.T) {
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
	mix := requireMix(t, got)
	if mix.Input != 2000 {
		t.Errorf("Mix.Input = %d, want 2000 (last token_count's input_tokens)", mix.Input)
	}
	if mix.Output != 200 {
		t.Errorf("Mix.Output = %d, want 200", mix.Output)
	}
	if mix.CacheRead != 300 {
		t.Errorf("Mix.CacheRead = %d, want 300 (cached_input_tokens)", mix.CacheRead)
	}
	if mix.CacheWrite != 0 {
		t.Errorf("Mix.CacheWrite = %d, want 0 (fixture has no cache_write_input_tokens)", mix.CacheWrite)
	}
}

// TestCodexUsageDaily: one session file -> one Daily entry, keyed by the LAST token_count's timestamp,
// localized; ActiveDays == 1.
func TestCodexUsageDaily(t *testing.T) {
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

	lastTS, err := time.Parse(time.RFC3339, "2026-10-23T10:15:00Z")
	if err != nil {
		t.Fatal(err)
	}
	wantDate := lastTS.Local().Format("2006-01-02")

	got := codexUsage(time.Now().Add(-24 * time.Hour))
	if got.ActiveDays != 1 {
		t.Fatalf("ActiveDays = %d, want 1", got.ActiveDays)
	}
	if len(got.Daily) != 1 {
		t.Fatalf("Daily = %+v, want 1 entry", got.Daily)
	}
	if got.Daily[0].Date != wantDate {
		t.Errorf("Daily[0].Date = %q, want %q (local day of the last token_count)", got.Daily[0].Date, wantDate)
	}
	if got.Daily[0].Tokens != 2500 {
		t.Errorf("Daily[0].Tokens = %d, want 2500", got.Daily[0].Tokens)
	}
}

// --- opencode: Mix + Daily + ActiveDays (NOT landed: opencodeUsage's return omits them entirely) ---

// TestOpencodeUsageMix: Mix should sum the totals row's input/output/cache_read/cache_write columns.
func TestOpencodeUsageMix(t *testing.T) {
	writeOpencodeStore(t)
	stubLookPathSqlite3(t, func() error { return nil })
	stubOpencodeSqliteWithDaily(t,
		readFixture(t, "opencode-totals-fixture.json"),
		readFixture(t, "opencode-models-fixture.json"),
		readFixture(t, "opencode-daily-fixture.json"))

	got := opencodeUsage(context.Background(), time.Now().Add(-24*time.Hour))
	mix := requireMix(t, got)
	if mix.Input != 200000 || mix.Output != 90000 || mix.CacheRead != 20866 || mix.CacheWrite != 10000 {
		t.Errorf("Mix = %+v, want {200000 90000 20866 10000}", mix)
	}
}

// TestOpencodeUsageDaily: two fixture days should become two ascending Daily entries, ActiveDays == 2,
// summing back to the totals row's 320866.
func TestOpencodeUsageDaily(t *testing.T) {
	writeOpencodeStore(t)
	stubLookPathSqlite3(t, func() error { return nil })
	stubOpencodeSqliteWithDaily(t,
		readFixture(t, "opencode-totals-fixture.json"),
		readFixture(t, "opencode-models-fixture.json"),
		readFixture(t, "opencode-daily-fixture.json"))

	got := opencodeUsage(context.Background(), time.Now().Add(-24*time.Hour))
	if got.ActiveDays != 2 {
		t.Fatalf("ActiveDays = %d, want 2", got.ActiveDays)
	}
	if len(got.Daily) != 2 {
		t.Fatalf("Daily = %+v, want 2 entries", got.Daily)
	}
	if got.Daily[0].Date != "2026-10-22" || got.Daily[1].Date != "2026-10-23" {
		t.Errorf("Daily dates = [%s %s], want ascending 2026-10-22, 2026-10-23", got.Daily[0].Date, got.Daily[1].Date)
	}
	sum := 0
	for _, d := range got.Daily {
		sum += d.Tokens
	}
	if sum != 320866 {
		t.Errorf("sum(Daily.Tokens) = %d, want 320866 (matches the totals row)", sum)
	}
}

// --- copilot: Mix + Daily + ActiveDays (NOT landed: no row types exist, return omits them entirely) ---

// TestCopilotUsageMix: Mix should be the same four columns copilotTokenSum already adds for Tokens,
// reported individually.
func TestCopilotUsageMix(t *testing.T) {
	writeCopilotStore(t)
	stubLookPathSqlite3(t, func() error { return nil })
	stubCopilotSqliteWithDaily(t,
		readFixture(t, "copilot-totals-fixture.json"),
		readFixture(t, "copilot-models-fixture.json"),
		readFixture(t, "copilot-daily-fixture.json"))

	got := copilotUsage(context.Background(), time.Now().Add(-24*time.Hour))
	mix := requireMix(t, got)
	if mix.Input != 1000000 || mix.Output != 500000 || mix.CacheRead != 150190 || mix.CacheWrite != 51000 {
		t.Errorf("Mix = %+v, want {1000000 500000 150190 51000}", mix)
	}
}

// TestCopilotUsageDaily: two fixture days (one turn each) should give ActiveDays == 2, ascending, tokens
// summing back to 1701190.
func TestCopilotUsageDaily(t *testing.T) {
	writeCopilotStore(t)
	stubLookPathSqlite3(t, func() error { return nil })
	stubCopilotSqliteWithDaily(t,
		readFixture(t, "copilot-totals-fixture.json"),
		readFixture(t, "copilot-models-fixture.json"),
		readFixture(t, "copilot-daily-fixture.json"))

	got := copilotUsage(context.Background(), time.Now().Add(-24*time.Hour))
	if got.ActiveDays != 2 {
		t.Fatalf("ActiveDays = %d, want 2", got.ActiveDays)
	}
	if len(got.Daily) != 2 || got.Daily[0].Date != "2026-10-22" || got.Daily[1].Date != "2026-10-23" {
		t.Errorf("Daily = %+v, want ascending 2026-10-22, 2026-10-23", got.Daily)
	}
	sum := 0
	for _, d := range got.Daily {
		sum += d.Tokens
	}
	if sum != 1701190 {
		t.Errorf("sum(Daily.Tokens) = %d, want 1701190", sum)
	}
}

// --- claude: Mix from the transcript aggregate, CacheCreation -> CacheWrite (landed) ---

// TestClaudeAgentUsageMix: Mix sums Input/Output/CacheRead/CacheCreation across every model in the
// aggregate, with CacheCreation landing in Mix.CacheWrite.
func TestClaudeAgentUsageMix(t *testing.T) {
	agg := transcript.NewUsageAggregate()
	agg.Models["sonnet"] = &transcript.ModelUsage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 5, CacheCreationTokens: 50}
	agg.Models["haiku"] = &transcript.ModelUsage{InputTokens: 900, OutputTokens: 10, CacheReadTokens: 1, CacheCreationTokens: 2}

	got := claudeAgentUsage(agg, 3, nil)
	mix := requireMix(t, got)
	want := tokenMix{Input: 1000, Output: 30, CacheRead: 6, CacheWrite: 52}
	if mix != want {
		t.Errorf("Mix = %+v, want %+v", mix, want)
	}
}

// --- zero-safety: Mix/Daily are omitted, but ActiveDays currently always renders (no omitempty tag) ---

// TestAgentUsageZeroFieldsOmitted: an agentUsage with no mix/daily data should marshal without any of
// those keys — the spec calls this zero-safe omitempty. Mix (nil *tokenMix) and Daily (nil slice) already
// omit correctly; ActiveDays has no `omitempty` tag on its `int` field, so it always renders "active_days":0.
func TestAgentUsageZeroFieldsOmitted(t *testing.T) {
	a := agentUsage{Agent: "codex", Note: "no sessions in window"}
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, `"mix"`) {
		t.Errorf("marshaled zero-value agentUsage contains \"mix\", want it omitted: %s", s)
	}
	if strings.Contains(s, `"daily"`) {
		t.Errorf("marshaled zero-value agentUsage contains \"daily\", want it omitted: %s", s)
	}
	if strings.Contains(s, `"active_days"`) {
		t.Errorf(`marshaled zero-value agentUsage contains "active_days" (missing omitempty on agentUsage.ActiveDays): %s`, s)
	}
}

// --- window_days: the --days flag surfaces at the top level of the usage JSON ---

// TestRunJSONWindowDaysMatchesFlag: `usage --format json --days N` carries window_days == N at the
// document's top level.
func TestRunJSONWindowDaysMatchesFlag(t *testing.T) {
	origQuotas := fetchQuotas
	fetchQuotas = func() []quota { return nil }
	t.Cleanup(func() { fetchQuotas = origQuotas })

	var out, errw bytes.Buffer
	if code := Run([]string{"--format", "json", "--days", "5"}, t.TempDir(), &out, &errw); code != 0 {
		t.Fatalf("Run = %d; stderr:\n%s", code, errw.String())
	}
	var doc struct {
		WindowDays int `json:"window_days"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out.String())
	}
	if doc.WindowDays != 5 {
		t.Errorf("window_days = %d, want 5", doc.WindowDays)
	}
}
