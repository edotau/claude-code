package tokenledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
)

// withLedgerDir points paths.ConfigDir (and so Path) at a scratch dir for the duration of the test.
func withLedgerDir(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
}

func TestAppendReadRoundTrip(t *testing.T) {
	withLedgerDir(t)
	ts := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	Append(Entry{Ts: ts, Provider: "openai", Model: "gpt-5", Input: 100, Output: 20, CacheRead: 5})
	Append(Entry{Ts: ts.Add(time.Minute), Provider: "anthropic", Model: "claude-opus-5", Input: 10, Output: 2})

	got, err := Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Read = %d entries, want 2: %+v", len(got), got)
	}
	if got[0].Provider != "openai" || got[0].Input != 100 || got[0].CacheRead != 5 {
		t.Errorf("entry 0 = %+v", got[0])
	}
	if got[1].Provider != "anthropic" || !got[1].Ts.Equal(ts.Add(time.Minute)) {
		t.Errorf("entry 1 = %+v", got[1])
	}
}

func TestReadMissingFileIsEmptyNotError(t *testing.T) {
	withLedgerDir(t)
	got, err := Read()
	if err != nil || got != nil {
		t.Errorf("Read on missing ledger = %v, %v; want nil, nil", got, err)
	}
}

// TestReadSkipsMalformedLines
func TestReadSkipsMalformedLines(t *testing.T) {
	withLedgerDir(t)
	good1 := `{"ts":"2026-10-20T00:00:00Z","provider":"openai","model":"gpt-5","input":1,"output":1,"cache_read":0}`
	bad := `{"ts":"2026-10-20T00:01` // truncated JSON
	good2 := `not json at all`
	good3 := `{"ts":"2026-10-20T00:02:00Z","provider":"anthropic","model":"claude-opus-5","input":2,"output":2,"cache_read":0}`
	content := good1 + "\n" + bad + "\n" + good2 + "\n" + good3 + "\n"
	if err := os.MkdirAll(filepath.Dir(Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Read = %d entries, want 2 (malformed/non-JSON lines skipped): %+v", len(got), got)
	}
	if got[0].Provider != "openai" || got[1].Provider != "anthropic" {
		t.Errorf("entries = %+v", got)
	}
}

// TestEntryFieldShape
func TestEntryFieldShape(t *testing.T) {
	b, err := json.Marshal(Entry{Ts: time.Now(), Provider: "openai", Model: "gpt-5", Input: 1, Output: 1, CacheRead: 1})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"schema": true, "ts": true, "provider": true, "model": true, "input": true, "output": true, "cache_read": true}
	full, _ := json.Marshal(Entry{Ts: time.Now(), Profile: "p", Provider: "openai", Agent: "a", Session: "s", Model: "m"})
	var fm map[string]json.RawMessage
	if err := json.Unmarshal(full, &fm); err != nil {
		t.Fatal(err)
	}
	t.Run("attribution", func(t *testing.T) {
		checkEntryAttribution(t, m, fm, b, full)
	})
	t.Run("required fields", func(t *testing.T) {
		checkEntryFields(t, m, want, b)
	})
}

func checkEntryAttribution(t *testing.T, m, fm map[string]json.RawMessage, b, full []byte) {
	t.Helper()
	for _, k := range []string{"profile", "agent", "session"} {
		if _, ok := fm[k]; !ok {
			t.Errorf("populated entry missing %q: %s", k, full)
		}
		if _, ok := m[k]; ok {
			t.Errorf("empty %q must be omitted: %s", k, b)
		}
	}
}

func checkEntryFields(t *testing.T, m map[string]json.RawMessage, want map[string]bool, b []byte) {
	t.Helper()
	for k := range m {
		if !want[k] {
			t.Errorf("ledger entry has unexpected field %q (secret/content leak risk): %s", k, b)
		}
	}
	for k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("ledger entry missing expected field %q: %s", k, b)
		}
	}
}

// TestAppendBestEffortOnUnwritableDir
func TestAppendBestEffortOnUnwritableDir(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(blocker, "sub"))
	Append(Entry{Ts: time.Now(), Provider: "openai", Model: "gpt-5", Input: 1})
	if got, err := os.ReadFile(blocker); err != nil || string(got) != "x" {
		t.Errorf("blocker = %q, %v; want it untouched", got, err)
	}
	if _, err := os.Stat(Path()); err == nil {
		t.Error("a ledger file exists under the blocked config dir")
	}
}

func TestOldLinesParseAndProfileName(t *testing.T) {
	withLedgerDir(t)
	p := Path()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = os.WriteFile(p, []byte(`{"ts":"2026-10-20T12:00:00Z","provider":"prod","model":"m","input":4,"output":1,"cache_read":0}`+"\n"), 0o600)
	got, err := Read()
	if err != nil || len(got) != 1 {
		t.Fatalf("Read = %v, %v", got, err)
	}
	if got[0].ProfileName() != "prod" || got[0].Session != "" || got[0].Agent != "" {
		t.Errorf("old entry = %+v", got[0])
	}
	if in, _, _, _ := SessionTotals(""); in != 0 {
		t.Error("empty id must match nothing")
	}
}

// TestUsageLedgerSchemaStampsNewLinesAndSkipsNewer pins: unstamped lines read, new lines carry schema 1, schema-2 lines are skipped.
func TestUsageLedgerSchemaStampsNewLinesAndSkipsNewer(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	seed := `{"ts":"2026-01-01T00:00:00Z","provider":"p","model":"old","input":1,"output":2,"cache_read":3}` + "\n" +
		`{"schema":2,"ts":"2026-01-02T00:00:00Z","provider":"p","model":"future","input":9,"output":9,"cache_read":9}` + "\n"
	if err := os.MkdirAll(filepath.Dir(Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	Append(Entry{Ts: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), Provider: "p", Model: "new", Input: 4})

	got, err := Read()
	if err != nil {
		t.Fatal(err)
	}
	var models []string
	for _, e := range got {
		models = append(models, e.Model)
	}
	if strings.Join(models, ",") != "old,new" {
		t.Errorf("models = %v, want [old new] (schema 2 skipped)", models)
	}
	b, _ := os.ReadFile(Path())
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, `"schema":1`) {
		t.Errorf("new line lacks schema 1: %s", last)
	}
}

func TestSessionTotalsIgnoresLinesOfOtherSessions(t *testing.T) {
	withLedgerDir(t)
	ts := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	Append(Entry{Ts: ts, Provider: "p", Model: "m", Session: "abc", Input: 1, Output: 2, CacheRead: 3})
	Append(Entry{Ts: ts, Provider: "p", Model: "m", Session: "abcd", Input: 50, Output: 50, CacheRead: 50})
	Append(Entry{Ts: ts, Provider: "abc", Model: `"abc"`, Session: "other", Input: 70})
	Append(Entry{Ts: ts, Provider: "p", Model: "m", Session: "abc", Input: 10, Output: 20, CacheRead: 30})
	in, out, cr, err := SessionTotals("abc")
	if err != nil || in != 11 || out != 22 || cr != 33 {
		t.Fatalf("SessionTotals = %d, %d, %d, %v; want 11, 22, 33, nil", in, out, cr, err)
	}
}

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

// TestAppendAfterTornLine: a fragment with no trailing newline must not swallow the next entry.
func TestAppendAfterTornLine(t *testing.T) {
	withLedgerDir(t)
	if err := os.MkdirAll(filepath.Dir(Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), []byte(`{"ts":"2026-10-20T00:01`), 0o600); err != nil {
		t.Fatal(err)
	}
	Append(Entry{Ts: time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC), Provider: "openai", Model: "gpt-5", Input: 7})
	got, err := Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 1 || got[0].Input != 7 {
		t.Fatalf("Read = %+v, want only the new entry", got)
	}
}

func BenchmarkRead(b *testing.B) {
	b.Setenv("CLAUDE_CONFIG_DIR", b.TempDir())
	ts := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for range 2000 {
		Append(Entry{Ts: ts, Provider: "openai", Model: "gpt-5", Session: "s", Input: 100, Output: 20})
	}
	for b.Loop() {
		if _, err := Read(); err != nil {
			b.Fatal(err)
		}
	}
}

// legacyEntry is the Entry shape before Requested existed: what an older binary decodes into.
type legacyEntry struct {
	Schema   int       `json:"schema"`
	Ts       time.Time `json:"ts"`
	Provider string    `json:"provider"`
	Model    string    `json:"model"`
	Input    int       `json:"input"`
}

func TestRequestedFieldIsCompatibleBothWays(t *testing.T) {
	withLedgerDir(t)
	ts := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	Append(Entry{Ts: ts, Provider: "p", Model: "m", Input: 1})
	Append(Entry{Ts: ts, Provider: "p", Model: "m", Requested: "asked", Input: 2})
	raw, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	checkOlderBinaryReads(t, lines)
	oldLine := `{"schema":1,"ts":"2026-10-20T12:00:00Z","provider":"p","model":"m","input":3,"output":0,"cache_read":0}` + "\n"
	if err := os.WriteFile(Path(), append(raw, oldLine...), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Read()
	if err != nil || len(got) != 3 || got[1].Requested != "asked" || got[2].Requested != "" || got[2].Input != 3 {
		t.Fatalf("Read = %+v, %v", got, err)
	}
}

func checkOlderBinaryReads(t *testing.T, lines []string) {
	t.Helper()
	if strings.Contains(lines[0], "requested") || !strings.Contains(lines[1], `"requested":"asked"`) {
		t.Fatalf("requested must be omitted when empty and written when set:\n%s", lines)
	}
	var old legacyEntry
	if err := json.Unmarshal([]byte(lines[1]), &old); err != nil || old.Model != "m" || old.Input != 2 {
		t.Fatalf("an older binary cannot read a new line: %+v, %v", old, err)
	}
}
