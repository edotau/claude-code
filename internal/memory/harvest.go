package memory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// HarvestPrompt is the headless harvest instruction; the tail bound keeps a multi-MB transcript from eating the turns.
const HarvestPrompt = "Background harvest of a finished session (transcript: %s). " +
	"Read ONLY the last ~1500 lines of that transcript — it may be many MB and reading it whole will " +
	"exhaust your turn budget before you can write anything. " +
	"Then run /memory:end — the COMPLETE pass, every step, not the abbreviated one. " +
	"Do it in this order so a truncated run still leaves the bank coherent: sessionHistory first, then " +
	"activeContext, then progress, decisionLog, and conventions. Be surgical; touch nothing else."

// Harvest statuses: pending = dispatched, not yet reported.
const (
	HarvestPending = "pending"
	HarvestOK      = "ok"
	HarvestFailed  = "failed"
)

// harvestGrace must exceed the worker's own timeout, or a healthy long harvest reads as lost.
const harvestGrace = 16 * time.Minute

// HarvestTimeout bounds the headless claude run.
const HarvestTimeout = 15 * time.Minute

// StateDir holds harvest stamps, results, logs and the session chain.
func StateDir() string { return filepath.Join(paths.StateDir(), "memory") }

// HarvestResult is one session's harvest outcome (KEY=value lines on disk).
type HarvestResult struct {
	Status      string
	Time        time.Time
	BankUpdated bool
	Log         string
}

func resultPath(sessionID string) string {
	return filepath.Join(StateDir(), "harvest-result-"+filepath.Base(sessionID))
}

// WriteHarvestResult persists r for sessionID; a blank id (manual run) records nothing.
func WriteHarvestResult(sessionID string, r HarvestResult) error {
	if sessionID == "" {
		return nil
	}
	body := "status=" + r.Status + "\ntime=" + r.Time.Format(time.RFC3339) +
		"\nbank_updated=" + strconv.FormatBool(r.BankUpdated) + "\nlog=" + r.Log + "\n"
	return paths.AtomicWrite(resultPath(sessionID), []byte(body), 0o600)
}

// ReadHarvestResult loads sessionID's result; ok=false when absent or unparseable.
func ReadHarvestResult(sessionID string) (HarvestResult, bool) {
	data, err := os.ReadFile(resultPath(sessionID))
	if err != nil {
		return HarvestResult{}, false
	}
	m := map[string]string{}
	for _, ln := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(ln, "="); ok {
			m[k] = v
		}
	}
	r := HarvestResult{Status: m["status"], Log: m["log"], BankUpdated: m["bank_updated"] == "true"}
	r.Time, _ = time.Parse(time.RFC3339, m["time"])
	return r, r.Status != ""
}

// NeedsAttention: a recorded failure, or a pending dispatch past the grace window (the worker died).
func (r HarvestResult) NeedsAttention(now time.Time) bool {
	return r.Status == HarvestFailed || (r.Status == HarvestPending && now.Sub(r.Time) > harvestGrace)
}

// UpdateOptions drive one background harvest.
type UpdateOptions struct {
	Claude     string // real claude binary; "" records a failure
	Transcript string
	CWD        string
	Session    string
	Timeout    time.Duration // 0 = HarvestTimeout
	Out        io.Writer
}

// Update runs headless /memory:end under the bank lock, settles the bank, and records the outcome.
func Update(o UpdateOptions) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Timeout == 0 {
		o.Timeout = HarvestTimeout
	}
	prior, _ := ReadHarvestResult(o.Session)
	record := func(status string, updated bool) {
		_ = WriteHarvestResult(o.Session, HarvestResult{Status: status, Time: time.Now(), BankUpdated: updated, Log: prior.Log})
	}
	if o.Claude == "" {
		record(HarvestFailed, false)
		return errors.New("claude binary not found")
	}
	dir := Dir(o.CWD)
	var before time.Time
	var runErr error
	lockErr := paths.WithFileLock(lockPath(dir), func() error {
		before = NewestMtime(dir)
		ctx, cancel := context.WithTimeout(context.Background(), o.Timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, o.Claude, "-p", fmt.Sprintf(HarvestPrompt, o.Transcript), "--max-turns", "40")
		cmd.Dir = o.CWD
		cmd.Env = append(os.Environ(), HarvestChildEnv+"=1")
		cmd.Stdout, cmd.Stderr = o.Out, o.Out
		runErr = cmd.Run()
		return nil
	})
	updated := NewestMtime(dir).After(before)
	if _, err := SettleBank(o.CWD, time.Now()); err != nil {
		fmt.Fprintln(o.Out, "memory update: settle:", err)
	}
	if err := errors.Join(lockErr, runErr); err != nil {
		record(HarvestFailed, updated)
		return fmt.Errorf("headless harvest: %w", err)
	}
	record(HarvestOK, updated)
	return nil
}
