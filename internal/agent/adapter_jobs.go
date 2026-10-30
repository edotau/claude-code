// adapter_jobs.go is background-job control on top of the dispatch log (claude-code ask --job).

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// executable is swapped by tests.
var executable = os.Executable

// Spawn re-execs claude-code detached to run the saved record: argv ["ask","--job",rec.ID] + extra;
// stdout+stderr -> rec.Log (0600); sets rec.PID in memory for the caller's print, but never saves —
// the caller already saved rec as "running" before calling Spawn, and the --job child saves its own
// PID once it starts, so a post-Start save here could overwrite a fast child's finished status.
func Spawn(rec *Record, extra []string) error {
	return SpawnArgv(rec, append([]string{"ask", "--job", rec.ID}, extra...), nil)
}

// SpawnArgv is Spawn for an arbitrary claude-code argv, with env appended to the child's environment.
func SpawnArgv(rec *Record, argv, env []string) error {
	exe, err := executable()
	if err != nil {
		return err
	}
	logPath := rec.LogPath()
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_APPEND, 0o600) // append: the job's sink writes too
	if err != nil {
		return err
	}
	defer logFile.Close()

	if proc.IsSelfUnderTest(exe) {
		return errors.New("agent: refusing to relaunch the test binary as a job")
	}
	cmd := exec.CommandContext(context.Background(), exe, argv...) // detached child must outlive this process, not any caller ctx
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Env = append(os.Environ(), env...)
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}

	rec.PID, rec.Log = cmd.Process.Pid, logPath
	go func() { _ = cmd.Wait() }() // reap the detached child; its exit status is read from the record, not here
	return nil
}

// RepoRoot is the main checkout root for cwd, falling back to cwd when git fails.
func RepoRoot(ctx context.Context, cwd string) string {
	out, err := proc.RunGit(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil || out == "" {
		return cwd
	}
	return paths.MainCheckout(strings.TrimSpace(out))
}

// List returns dispatch records for agentName, newest first; all=false keeps only Repo==repo.
func List(agentName, repo string, all bool) []Record {
	var out []Record
	for _, r := range RecentRecords(1 << 20) {
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
func Resolve(recs []Record, ref string, pred func(Record) bool) (Record, error) {
	if ref != "" {
		var match []Record
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
			return Record{}, fmt.Errorf("no dispatch matches %q (see claude-code <agent> status)", ref)
		default:
			return Record{}, fmt.Errorf("%q matches %d dispatches, be more specific (see claude-code <agent> status)", ref, len(match))
		}
	}
	for _, r := range recs {
		if pred == nil || pred(r) {
			return r, nil
		}
	}
	return Record{}, errors.New("no matching dispatch found (see claude-code <agent> status)")
}

// Alive reports a running record whose PID is still alive.
func Alive(rec Record) bool {
	return rec.Alive()
}

// procArgv returns pid's argv via /proc on Linux (NUL-split), falling back to `ps -o command=` split on spaces.
func procArgv(pid int) ([]string, error) {
	if raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		return strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00"), nil
	}
	out, err := exec.CommandContext(context.Background(), "ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// jobArgvSettled reads pid's argv, retrying for ~500ms while it does not match: a child spawned a moment ago
// has forked but not yet exec'd, so /proc still shows the parent's argv.
func jobArgvSettled(pid int, id string) ([]string, error) {
	var argv []string
	var err error
	for range 5 {
		if argv, err = procArgv(pid); err != nil || isJobArgv(argv, id) {
			return argv, err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return argv, nil
}

// isJobArgv reports whether argv carries the whole arguments `ask`, `--job` and id.
func isJobArgv(argv []string, id string) bool {
	return slices.Contains(argv, "ask") && slices.Contains(argv, "--job") && slices.Contains(argv, id)
}

// markGone reloads rec and marks it Status "error" only if it is still "running".
func markGone(rec *Record) error {
	if latest, err := LoadRecord(rec.ID); err == nil {
		*rec = latest
	}
	if rec.Status != "running" {
		return nil
	}
	rec.Status, rec.Error = "error", "process gone (pid reused)"
	return rec.Save()
}

// Cancel verifies rec.PID is still the job it was spawned as (guards against pid reuse), interrupts it
// (SIGINT), waits up to 5s, escalates to SIGKILL and waits up to 2s more, and only once the process is
// gone marks a still-"running" record Status "error", Error "cancelled by claude-code <agent> cancel".
func Cancel(rec *Record) error {
	if rec.PID <= 0 {
		return fmt.Errorf("dispatch %q has no pid to cancel", rec.ID)
	}
	if !proc.Alive(rec.PID) {
		return markGone(rec)
	}
	argv, err := jobArgvSettled(rec.PID, rec.ID)
	if err != nil {
		return fmt.Errorf("dispatch %q: cannot read pid %d command line: %w", rec.ID, rec.PID, err)
	}
	if !isJobArgv(argv, rec.ID) {
		if rec.Log == "" || (slices.Contains(argv, "ask") && !slices.Contains(argv, "--job")) { // a foreground ask
			return fmt.Errorf("dispatch %q is a foreground run; interrupt it in its own terminal", rec.ID)
		}
		return fmt.Errorf("dispatch %q: pid %d is a different program (pid reused); not signalling it", rec.ID, rec.PID)
	}
	if err := escalate(rec.PID, rec.ID); err != nil {
		return err
	}
	if latest, err := LoadRecord(rec.ID); err == nil {
		*rec = latest
	}
	if rec.Status == "running" {
		rec.Status, rec.Error = "error", fmt.Sprintf("cancelled by claude-code %s cancel", rec.Agent)
		return rec.Save()
	}
	return nil
}

// cancelGrace is how long escalate waits after SIGINT before sending SIGKILL; a var so tests can shorten it.
var cancelGrace = 5 * time.Second

// killWait is how long escalate waits after SIGKILL before reporting the pid as stuck; tests shorten it.
var killWait = 2 * time.Second

// escalate sends SIGINT, waits cancelGrace, then SIGKILL and waits killWait more; errors if pid outlives both.
func escalate(pid int, id string) error {
	if err := interrupt(pid); err != nil {
		return err
	}
	if waitGone(pid, cancelGrace) {
		return nil
	}
	if err := kill(pid); err != nil {
		return err
	}
	if !waitGone(pid, killWait) {
		return fmt.Errorf("dispatch %q still running after SIGKILL", id)
	}
	return nil
}

// waitGone polls up to d for pid to stop being alive.
func waitGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !proc.Alive(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !proc.Alive(pid)
}
