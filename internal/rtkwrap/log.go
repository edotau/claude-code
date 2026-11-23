package rtkwrap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// logCap is the size at which the log rotates to LogFile()+".1".
const logCap = 16 << 20

// LogFile is the trial's decision log: one JSON line per shim call; argv values and non-flag words are never kept.
func LogFile() string {
	return filepath.Join(paths.StateDir(), "rtkwrap", "decisions.jsonl")
}

type decision struct {
	T        string   `json:"t"`
	Name     string   `json:"name"`
	Verdict  string   `json:"verdict"`
	Reason   string   `json:"reason,omitempty"`
	Dispatch string   `json:"dispatch"`
	Cwd      string   `json:"cwd"`
	Flags    []string `json:"flags"`
	Argc     int      `json:"argc"`
}

// logFlags keeps the first 12 argv flag names (32 chars max, =value dropped): a command line can carry secrets.
func logFlags(args []string) []string {
	flags := []string{}
	for _, a := range args {
		if len(flags) == 12 {
			break
		}
		if !strings.HasPrefix(a, "-") {
			continue
		}
		a, _, _ = strings.Cut(a, "=")
		if !strings.HasPrefix(a, "--") {
			a = a[:min(len(a), 2)] // a short flag may carry its value attached (-psecret)
		}
		flags = append(flags, a[:min(len(a), 32)])
	}
	return flags
}

// logDecision appends one line best effort; reason "" means the call wraps (debug: would-wrap, on: wrapped).
func logDecision(mode, name, reason string, args []string) {
	if mode != "on" && mode != "debug" {
		return
	}
	verdict := "raw"
	if reason == "" {
		verdict = map[string]string{"on": "wrapped", "debug": "would-wrap"}[mode]
	}
	logVerdict(name, verdict, reason, args)
}

// logVerdict appends one decision line best effort.
func logVerdict(name, verdict, reason string, args []string) {
	cwd, _ := os.Getwd()
	d := decision{T: time.Now().UTC().Format(time.RFC3339), Name: name, Verdict: verdict, Reason: reason,
		Dispatch: os.Getenv(proc.DispatchIDEnv), Cwd: paths.ShortHome(cwd), Flags: logFlags(args), Argc: len(args)}
	line, err := json.Marshal(d)
	if err != nil {
		return
	}
	file := LogFile()
	if fi, err := os.Stat(file); err == nil && fi.Size() >= logCap {
		_ = os.Rename(file, file+".1") // one generation kept; racing shims may drop it, never the live log
	}
	_ = os.MkdirAll(filepath.Dir(file), 0o700)
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n')) // one O_APPEND write per line, so concurrent shims never interleave
	_ = f.Close()
}
