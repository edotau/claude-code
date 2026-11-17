package core

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
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

// TelemetryRow is one module run: names, wall time and outcome, plus the payload's session id and tool name.
type TelemetryRow struct {
	Event, Module, Outcome string
	Dur                    time.Duration
	Sid, Tool              string
}

type telemetryLine struct {
	T       string  `json:"t"`
	Event   string  `json:"event"`
	Module  string  `json:"module"`
	Ms      float64 `json:"ms"`
	Outcome string  `json:"outcome"`
	Sid     string  `json:"sid,omitempty"`
	Tool    string  `json:"tool,omitempty"`
}

const telemetrySidLen = 8

// telemetryDir is state/harness/hooks; a test binary writes nothing unless TelemetryDirForTest names a directory.
var telemetryDir = func() string {
	if testing.Testing() {
		return ""
	}
	return filepath.Join(paths.StateDir(), "hooks")
}

// TelemetryDirForTest redirects the telemetry directory until the returned restore runs.
func TelemetryDirForTest(dir string) (restore func()) {
	prev, prevWait := telemetryDir, telemetryWait
	telemetryDir, telemetryWait = func() string { return dir }, 2*time.Second // tests count rows: never drop them
	return func() { telemetryDir, telemetryWait = prev, prevWait }
}

// TelemetryWaitForTest sets the hook's telemetry wait, for tests that stall the writer on purpose.
func TelemetryWaitForTest(d time.Duration) (restore func()) {
	prev := telemetryWait
	telemetryWait = d
	return func() { telemetryWait = prev }
}

const telemetryDailyBytes = 4 << 20
const telemetryDateLayout = "20060102"
const telemetryRetentionDays = 14

// telemetryWait bounds the disk write a hook waits for; tests widen it.
var telemetryWait = 10 * time.Millisecond

var telemetryBusy atomic.Bool

// RecordTelemetry bounds best-effort disk work; a stalled filesystem never holds up a verdict.
func RecordTelemetry(rows []TelemetryRow) {
	dir := telemetryDir()
	if len(rows) == 0 || dir == "" || os.Getenv(telemetryEnv) == "off" || !telemetryBusy.CompareAndSwap(false, true) {
		return
	}
	rows = append([]TelemetryRow(nil), rows...)
	done := make(chan struct{})
	deadline := time.Now().Add(telemetryWait)
	go func() {
		defer close(done)
		defer telemetryBusy.Store(false)
		recordTelemetry(dir, rows, time.Now(), deadline)
	}()
	select {
	case <-done:
	case <-time.After(telemetryWait):
	}
}

func recordTelemetry(dir string, rows []TelemetryRow, now, deadline time.Time) {
	file := filepath.Join(dir, "telemetry-"+now.Format(telemetryDateLayout)+".jsonl")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	lock, err := os.OpenFile(file+".lock", os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = lock.Close() }()
	if !paths.TryLock(lock) {
		return
	}
	for _, r := range rows {
		if time.Now().After(deadline) {
			return
		}
		line, err := json.Marshal(telemetryLine{now.Format(time.RFC3339), r.Event, r.Module, float64(r.Dur) / float64(time.Millisecond), r.Outcome,
			r.Sid[:min(len(r.Sid), telemetrySidLen)], r.Tool})
		if err != nil {
			return
		}
		if err := appendTelemetry(file, append(line, '\n')); err != nil {
			return
		}
	}
	pruneTelemetry(dir, now)
}

// pruneTelemetry uses a daily marker shared by hook processes; only dated telemetry files expire.
func pruneTelemetry(dir string, now time.Time) {
	marker := filepath.Join(dir, "telemetry-"+now.Format(telemetryDateLayout)+".pruned")
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	if err := f.Close(); err != nil {
		return
	}
	entries, err := filepath.Glob(filepath.Join(dir, "telemetry-*"))
	if err != nil {
		return
	}
	cutoff := now.AddDate(0, 0, -telemetryRetentionDays).Format(telemetryDateLayout)
	for _, e := range entries {
		name := filepath.Base(e)
		if len(name) < len("telemetry-"+telemetryDateLayout+".jsonl") {
			continue
		}
		date, err := time.Parse(telemetryDateLayout, name[10:18])
		if err != nil || date.Format(telemetryDateLayout) >= cutoff {
			continue
		}
		switch name[18:] {
		case ".jsonl", ".jsonl.lock", ".pruned":
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

// appendTelemetry holds the daily lock across its size check, append and short-write rollback.
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
	st, err := f.Stat()
	if err != nil {
		return errors.Join(err, f.Close())
	}
	if !st.Mode().IsRegular() || st.Size()+int64(len(buf)) > telemetryDailyBytes {
		return f.Close()
	}
	n, werr := f.Write(buf)
	if n != len(buf) {
		werr = errors.Join(werr, io.ErrShortWrite, f.Truncate(st.Size()))
	}
	return errors.Join(werr, f.Close())
}
