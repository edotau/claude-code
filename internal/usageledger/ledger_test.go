package usageledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withLedgerDir points paths.ConfigDir (and so Path) at a scratch dir for the duration of the test.
func withLedgerDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	return dir
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

// TestReadSkipsMalformedLines: a truncated/garbage line is dropped, not failed on; well-formed
// lines around it still come through.
func TestReadSkipsMalformedLines(t *testing.T) {
	dir := withLedgerDir(t)
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
	_ = dir

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

// TestEntryFieldShape: the on-disk line carries only ts/provider/model/input/output/cache_read —
// never a credential, header or prompt/response content.
func TestEntryFieldShape(t *testing.T) {
	b, err := json.Marshal(Entry{Ts: time.Now(), Provider: "openai", Model: "gpt-5", Input: 1, Output: 1, CacheRead: 1})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"ts": true, "provider": true, "model": true, "input": true, "output": true, "cache_read": true}
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

// TestAppendBestEffortOnUnwritableDir: Append is a no-op, not a panic, when the config dir path is
// blocked by an existing plain file (os.MkdirAll fails).
func TestAppendBestEffortOnUnwritableDir(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(blocker, "sub"))
	Append(Entry{Ts: time.Now(), Provider: "openai", Model: "gpt-5", Input: 1})
}
