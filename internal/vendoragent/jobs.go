// Package vendoragent is background-job control on top of the dispatch log (claude-code ask --job).
package vendoragent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/gitexec"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// executable is swapped by tests.
var executable = os.Executable

// Spawn re-execs claude-code detached to run the saved record: argv ["ask","--job",rec.ID] + extra;
// stdout+stderr -> rec.Log (0600); sets rec.PID and saves.
func Spawn(rec *agent.Record, extra []string) error {
	exe, err := executable()
	if err != nil {
		return err
	}
	logPath := rec.Log
	if logPath == "" {
		logPath = agent.DispatchDir() + "/" + rec.ID + ".log"
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()

	argv := append([]string{"ask", "--job", rec.ID}, extra...)
	cmd := exec.Command(exe, argv...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}

	rec.PID, rec.Log = cmd.Process.Pid, logPath
	go func() { _ = cmd.Wait() }() // reap the detached child; its exit status is read from the record, not here
	return rec.Save()
}

// RepoRoot is the main checkout root for cwd, falling back to cwd when git fails.
func RepoRoot(ctx context.Context, cwd string) string {
	out, err := gitexec.RunGit(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil || out == "" {
		return cwd
	}
	return paths.MainCheckout(strings.TrimSpace(out))
}

// List returns dispatch records for agentName, newest first; all=false keeps only Repo==repo.
func List(agentName, repo string, all bool) []agent.Record {
	var out []agent.Record
	for _, r := range agent.RecentRecords(1 << 20) {
		if r.Agent != agentName {
			continue
		}
		if !all && r.Repo != repo {
			continue
		}
		out = append(out, r)
	}
	return out
}

// Resolve picks by exact id, unique id prefix, else the newest record matching pred.
func Resolve(recs []agent.Record, ref string, pred func(agent.Record) bool) (agent.Record, error) {
	if ref != "" {
		var match []agent.Record
		for _, r := range recs {
			if r.ID == ref {
				return r, nil
			}
			if strings.HasPrefix(r.ID, ref) {
				match = append(match, r)
			}
		}
		switch len(match) {
		case 1:
			return match[0], nil
		case 0:
			return agent.Record{}, fmt.Errorf("no dispatch matches %q (see claude-code <agent> status)", ref)
		default:
			return agent.Record{}, fmt.Errorf("%q matches %d dispatches, be more specific (see claude-code <agent> status)", ref, len(match))
		}
	}
	for _, r := range recs {
		if pred == nil || pred(r) {
			return r, nil
		}
	}
	return agent.Record{}, fmt.Errorf("no matching dispatch found (see claude-code <agent> status)")
}

// Alive reports a running record whose PID is still alive.
func Alive(rec agent.Record) bool {
	return rec.Status == "running" && proc.Alive(rec.PID)
}

// Cancel interrupts rec.PID (SIGINT), waits up to 5s for exit, and if the record still says running
// marks it Status "error", Error "cancelled by claude-code <agent> cancel", saved.
func Cancel(rec *agent.Record) error {
	if rec.PID <= 0 {
		return fmt.Errorf("dispatch %q has no pid to cancel", rec.ID)
	}
	if err := interrupt(rec.PID); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && proc.Alive(rec.PID) {
		time.Sleep(100 * time.Millisecond)
	}
	if latest, err := agent.LoadRecord(rec.ID); err == nil {
		*rec = latest
	}
	if rec.Status == "running" {
		rec.Status, rec.Error = "error", fmt.Sprintf("cancelled by claude-code %s cancel", rec.Agent)
		return rec.Save()
	}
	return nil
}
