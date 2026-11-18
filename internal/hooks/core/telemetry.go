package core

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	// OutcomeDropped marks the point a full day file stopped recording; later rows that day are missing.
	OutcomeDropped = "dropped"
)

// Verdict words a TelemetryRow reports for a decision; a hand-back is held, refused or passed, never both.
const (
	VerdictBlock   = "block"
	VerdictAsk     = "ask"
	VerdictRewrite = "rewrite"
	VerdictHeld    = "held"
	VerdictRefused = "refused"
	VerdictPassed  = "passed"
)

// TelemetryRow is one module run; Rule and Verdict are set only when a decision fired, never call text.
type TelemetryRow struct {
	Event, Module, Outcome string
	Dur                    time.Duration
	Sid, Tool              string
	Rule, Verdict          string
}

type telemetryLine struct {
	T       string  `json:"t"`
	Event   string  `json:"event"`
	Module  string  `json:"module"`
	Ms      float64 `json:"ms"`
	Outcome string  `json:"outcome"`
	Sid     string  `json:"sid,omitempty"`
	Tool    string  `json:"tool,omitempty"`
	Rule    string  `json:"rule,omitempty"`
	Verdict string  `json:"verdict,omitempty"`
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

// A heavy day wrote 4 MiB by 10:08 (~138 B/row), so 16 MiB spans one; the total cap bounds all retained days.
const telemetryDailyBytes = 16 << 20
const telemetryTotalBytes = 128 << 20
const telemetryMarkerReserve = 1 << 10
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
			r.Sid[:min(len(r.Sid), telemetrySidLen)], r.Tool, r.Rule, r.Verdict})
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
		case ".jsonl", ".jsonl.lock", ".pruned", ".dropped":
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
	trimTelemetryBytes(dir, now)
}

// trimTelemetryBytes removes the oldest day files until the rest fit telemetryTotalBytes; today's stays.
func trimTelemetryBytes(dir string, now time.Time) {
	files, err := filepath.Glob(filepath.Join(dir, "telemetry-????????.jsonl"))
	if err != nil {
		return
	}
	var total int64
	sizes := make([]int64, len(files))
	for i, f := range files {
		if st, err := os.Stat(f); err == nil {
			sizes[i] = st.Size()
			total += sizes[i]
		}
	}
	today := "telemetry-" + now.Format(telemetryDateLayout) + ".jsonl"
	for i, f := range files {
		if total <= telemetryTotalBytes || filepath.Base(f) >= today {
			return
		}
		if os.Remove(f) == nil {
			total -= sizes[i]
		}
	}
}

// telemetryDropMarker is the one row a full day file gets, once (an O_EXCL .dropped file); nil when already marked.
func telemetryDropMarker(file string) []byte {
	mk, err := os.OpenFile(strings.TrimSuffix(file, ".jsonl")+".dropped", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil
	}
	_ = mk.Close()
	line, err := json.Marshal(telemetryLine{T: time.Now().Format(time.RFC3339), Event: "telemetry", Module: "telemetry", Outcome: OutcomeDropped})
	if err != nil {
		return nil
	}
	return append(line, '\n')
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
	if !st.Mode().IsRegular() {
		return f.Close()
	}
	if st.Size()+int64(len(buf)) > telemetryDailyBytes-telemetryMarkerReserve {
		if buf = telemetryDropMarker(file); buf == nil || st.Size()+int64(len(buf)) > telemetryDailyBytes {
			return f.Close()
		}
	}
	n, werr := f.Write(buf)
	if n != len(buf) {
		werr = errors.Join(werr, io.ErrShortWrite, f.Truncate(st.Size()))
	}
	return errors.Join(werr, f.Close())
}
