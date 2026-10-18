package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// State is the record of the last adopt (state/harness/setup.json); the doctor row reads it back.
type State struct {
	Mode       string    `json:"mode"`
	Date       time.Time `json:"date"`
	Commit     string    `json:"commit"`
	BackupDir  string    `json:"backupDir"`
	ClashCount int       `json:"clashCount"`
}

func stateFile() string { return filepath.Join(paths.StateDir(), "setup.json") }

// Record persists st, overwriting any prior record.
func Record(st State) error {
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
	if json.Unmarshal(data, &st) != nil {
		return st, false
	}
	return st, true
}
