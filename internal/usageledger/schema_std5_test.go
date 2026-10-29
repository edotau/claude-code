package usageledger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/schemas"
)

// TestUsageLedgerSchemaStd5 pins: unstamped lines read, new lines carry schema 1, schema-2 lines are skipped.
func TestUsageLedgerSchemaStd5(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	found := false
	for _, f := range schemas.All() {
		found = found || f.Name == "usage-ledger"
	}
	if !found {
		t.Error(`schemas.All() has no "usage-ledger" row`)
	}

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
