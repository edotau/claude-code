package router

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

const v0Quota = `{"anthropic·a":{"used_pct":42.5,"window":"5h","seen":"2026-10-28T10:00:00Z"},` +
	`"codex·b":{"used_pct":10,"seen":"2026-10-28T11:00:00Z"}}`

func quotaFixture(t *testing.T, body string) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(paths.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(StatePath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return StatePath()
}

func wantEntries(t *testing.T, s State) {
	t.Helper()
	if len(s) != 2 || s["anthropic·a"].UsedPct != 42.5 || s["anthropic·a"].Window != "5h" || s["codex·b"].UsedPct != 10 {
		t.Fatalf("entries = %+v", s)
	}
}

// A v0 bare-map quota.json reads, install-time migration leaves it untouched (an old daemon still reads it), the
// {schema,accounts} wrapper reads too, and Update writes the bare map back.
func TestQuotaV0ReadsAndStaysBareMap(t *testing.T) {
	path := quotaFixture(t, v0Quota)
	wantEntries(t, LoadState())

	if _, err := schemas.MigrateAll(false); err != nil {
		t.Fatalf("MigrateAll: %v", err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != v0Quota {
		t.Fatalf("quota.json rewritten: %s", raw)
	}
	if _, err := os.Stat(filepath.Join(paths.StateDir(), "migrations.jsonl")); err == nil {
		t.Fatal("migration ledger written for quota")
	}

	wrapped := `{"schema":1,"accounts":` + v0Quota + `}`
	if err := os.WriteFile(path, []byte(wrapped), 0o600); err != nil {
		t.Fatal(err)
	}
	wantEntries(t, LoadState())
	if err := Update(func(State) {}); err != nil {
		t.Fatal(err)
	}
	var bare map[string]Entry
	if raw, _ := os.ReadFile(path); json.Unmarshal(raw, &bare) != nil || len(bare) != 2 || bare["codex·b"].UsedPct != 10 {
		t.Fatalf("Update wrote %s, want the bare map", raw)
	}
}

// A newer-schema document reads as empty but Update refuses and leaves the file byte-identical.
func TestQuotaNewerSchemaIsNotOverwritten(t *testing.T) {
	const newer = `{"schema":2,"accounts":{"a":{"used_pct":1,"seen":"2026-10-28T10:00:00Z"}}}`
	path := quotaFixture(t, newer)
	if len(LoadState()) != 0 {
		t.Fatal("newer document should read empty")
	}
	if err := Record("x", 5, "5h", time.Time{}, time.Now()); !errors.Is(err, schemas.ErrNewer) {
		t.Fatalf("Update err = %v, want ErrNewer", err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != newer {
		t.Fatalf("newer file overwritten: %s", raw)
	}
}
