package core

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// telemetryEnv set to "off" stops RecordTelemetry; read only from the hook process's own environment.
const telemetryEnv = "HARNESS_HOOK_TELEMETRY"

// Outcomes a TelemetryRow reports; Ask counts as deny because Claude renders it as one.
const (
	OutcomeOK      = "ok"
	OutcomeDeny    = "deny"
	OutcomeBlock   = "block"
	OutcomeOverrun = "overrun"
	OutcomePanic   = "panic"
)

// TelemetryRow is one module run: names, wall time and outcome only, never command text or payload.
type TelemetryRow struct {
	Event, Module, Outcome string
	Dur                    time.Duration
}

type telemetryLine struct {
	T       string  `json:"t"`
	Event   string  `json:"event"`
	Module  string  `json:"module"`
	Ms      float64 `json:"ms"`
	Outcome string  `json:"outcome"`
}

// telemetryDir is state/harness/hooks; a test binary writes nothing unless TelemetryDirForTest names a directory.
var telemetryDir = func() string {
	if testing.Testing() {
		return ""
	}
	return filepath.Join(paths.StateDir(), "hooks")
}

// TelemetryDirForTest redirects the telemetry directory until the returned restore runs.
func TelemetryDirForTest(dir string) (restore func()) {
	prev := telemetryDir
	telemetryDir = func() string { return dir }
	return func() { telemetryDir = prev }
}

// RecordTelemetry appends one JSON line per row to telemetry-YYYYMMDD.jsonl in one unlocked, unsynced write; fail-open.
func RecordTelemetry(rows []TelemetryRow) {
	dir := telemetryDir()
	if len(rows) == 0 || dir == "" || os.Getenv(telemetryEnv) == "off" {
		return
	}
	now := time.Now()
	var buf []byte
	for _, r := range rows {
		line, err := json.Marshal(telemetryLine{now.Format(time.RFC3339), r.Event, r.Module, float64(r.Dur.Microseconds()) / 1000, r.Outcome})
		if err != nil {
			return
		}
		buf = append(append(buf, line...), '\n')
	}
	_ = appendTelemetry(filepath.Join(dir, "telemetry-"+now.Format("20060102")+".jsonl"), buf)
}

// appendTelemetry writes buf with O_APPEND (one write, atomic between processes), creating the directory on demand.
func appendTelemetry(file string, buf []byte) error {
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		if err = os.MkdirAll(filepath.Dir(file), 0o700); err == nil {
			f, err = os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		}
	}
	if err != nil {
		return err
	}
	_, werr := f.Write(buf)
	return errors.Join(werr, f.Close())
}
