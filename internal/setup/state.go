package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// State is the record of the last adopt (state/harness/setup.json); the doctor row reads it back.
type State struct {
	Schema     int       `json:"schema"`
	Mode       string    `json:"mode"`
	Date       time.Time `json:"date"`
	Commit     string    `json:"commit"`
	BackupDir  string    `json:"backupDir"`
	ClashCount int       `json:"clashCount"`
}

func stateFile() string { return filepath.Join(paths.StateDir(), "setup.json") }

// Record persists st, overwriting any prior record.
func Record(st State) error {
	st.Schema = stateSchema
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return paths.AtomicWrite(stateFile(), data, 0o600)
}

// Read returns the last recorded state; ok is false when none exists or it is unreadable.
func Read() (State, bool) {
	var st State
	data, err := os.ReadFile(stateFile())
	if err != nil {
		return st, false
	}
	if _, err := schemas.Decode(setupFormat, data, &st); err != nil {
		return st, false
	}
	return st, true
}

// stateSchema versions setup-state.
const stateSchema = 1

var setupFormat = schemas.Register(schemas.Format{
	Name: "setup-state", Owner: "internal/setup", Kind: schemas.Record, Version: stateSchema,
	Glob:    func() string { return stateFile() },
	New:     func() any { return new(State) },
	Keep:    "last adopt record",
	Migrate: schemas.SameShape,
})
