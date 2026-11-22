package rtkwrap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// logCap stops appending past this size, like hook telemetry's daily cap.
const logCap = 16 << 20

// LogFile is the trial's decision log: one JSON line per shim call, command name and verdict only.
func LogFile() string {
	return filepath.Join(paths.StateDir(), "rtkwrap", "decisions.jsonl")
}

type decision struct {
	T       string `json:"t"`
	Name    string `json:"name"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason,omitempty"`
}

// logDecision appends one line best effort; reason "" means the call wraps (debug: would-wrap, on: wrapped).
func logDecision(mode, name, reason string) {
	if mode != "on" && mode != "debug" {
		return
	}
	verdict := "raw"
	if reason == "" {
		verdict = map[string]string{"on": "wrapped", "debug": "would-wrap"}[mode]
	}
	line, err := json.Marshal(decision{T: time.Now().UTC().Format(time.RFC3339), Name: name, Verdict: verdict, Reason: reason})
	if err != nil {
		return
	}
	file := LogFile()
	if fi, err := os.Stat(file); err == nil && fi.Size() >= logCap {
		return
	}
	_ = os.MkdirAll(filepath.Dir(file), 0o700)
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
}
