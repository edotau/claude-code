package router

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// A v0 bare-map quota.json reads, migrates to {schema,accounts} with one ledger line, and an old daemon's
// bare-map write after migration still reads.
func TestQuotaV0ReadsMigratesAndLateBareWriteReads(t *testing.T) {
	path := quotaFixture(t, v0Quota)
	wantEntries(t, LoadState())

	if _, err := schemas.MigrateAll(false); err != nil {
		t.Fatalf("MigrateAll: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Schema   int              `json:"schema"`
		Accounts map[string]Entry `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Schema != 1 || len(doc.Accounts) != 2 {
		t.Fatalf("migrated file = %s (%v)", raw, err)
	}
	wantEntries(t, State(doc.Accounts))
	wantEntries(t, LoadState())

	ledger, err := os.ReadFile(filepath.Join(paths.StateDir(), "migrations.jsonl"))
	if err != nil {
		t.Fatalf("no ledger: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(ledger)), "\n")
	var row struct {
		Format string `json:"format"`
		From   int    `json:"from"`
		To     int    `json:"to"`
	}
	if len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &row) != nil || row.Format != "quota" || row.From != 0 || row.To != 1 {
		t.Fatalf("ledger = %q", ledger)
	}

	if err := os.WriteFile(path, []byte(v0Quota), 0o600); err != nil { // an old daemon rewrites the bare map
		t.Fatal(err)
	}
	wantEntries(t, LoadState())
}
