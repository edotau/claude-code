package schemas

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

var _ = Register(Format{
	Name: "migrations", Owner: "schemas", Kind: Log, Glob: LedgerPath, Version: 1,
	Keep: "tiny audit trail of schema upgrades",
})

type ledgerRow struct {
	Schema  int    `json:"schema"`
	At      string `json:"at"`
	Format  string `json:"format"`
	From    int    `json:"from"`
	To      int    `json:"to"`
	Records int    `json:"records"`
}

// LedgerPath is the migration ledger file.
func LedgerPath() string { return filepath.Join(paths.StateDir(), "migrations.jsonl") }

// LogMigration appends one ledger line; fail-open: a write error never reaches the caller.
func LogMigration(format string, from, to, records int) {
	at := time.Now().UTC().Format(time.RFC3339)
	line, err := json.Marshal(ledgerRow{Schema: 1, At: at, Format: format, From: from, To: to, Records: records})
	if err != nil {
		return
	}
	p := LedgerPath()
	if os.MkdirAll(filepath.Dir(p), 0o755) != nil {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
}
