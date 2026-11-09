// job.go is background-job control on top of the dispatch log (claude-code agents ask --job).

package job

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/shapes"
)

// RepoRoot is the main checkout root for cwd, falling back to cwd when git fails.
func RepoRoot(ctx context.Context, cwd string) string {
	out, err := proc.RunGit(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil || out == "" {
		return cwd
	}
	return paths.MainCheckout(strings.TrimSpace(out))
}

// executable is swapped by tests.
var executable = os.Executable

// Spawn re-execs claude-code detached to run the saved record: argv ["agents","ask","--job",rec.ID] + extra;
// stdout+stderr -> rec.Log (0600); sets rec.PID in memory for the caller's print, but never saves —
// the caller already saved rec as "running" before calling Spawn, and the --job child saves its own
// PID once it starts, so a post-Start save here could overwrite a fast child's finished status.
func Spawn(rec *Record, extra []string) error {
	return SpawnArgv(rec, append([]string{"agents", "ask", "--job", rec.ID}, extra...), nil)
}

// SpawnArgv is Spawn for an arbitrary claude-code argv, with env appended to the child's environment.
func SpawnArgv(rec *Record, argv, env []string) error {
	exe, err := executable()
	if err != nil {
		return err
	}
	if proc.IsSelfUnderTest(exe) {
		return errors.New("agent: refusing to relaunch the test binary as a job")
	}
	logPath := rec.LogPath()
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_APPEND, 0o600) // append: the job's sink writes too
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.CommandContext(context.Background(), exe, argv...) // detached child must outlive this process, not any caller ctx
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Env = (agent.EnvSpec{Mode: agent.EnvClaude, Extra: env}).Environ(context.Background())
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}

	rec.PID, rec.Log = cmd.Process.Pid, logPath
	rec.PIDStart, _ = proc.StartTime(rec.PID) // "" on failure: Cancel then refuses to signal this pid
	go func() { _ = cmd.Wait() }()            // reap the detached child; its exit status is read from the record, not here
	return nil
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

// jobArgvSettled reads rec's pid argv, retrying for ~500ms while it does not match: a child spawned a moment ago
// has forked but not yet exec'd, so /proc still shows the parent's argv.
func jobArgvSettled(rec *Record) ([]string, error) {
	var argv []string
	var err error
	for range 5 {
		if argv, err = proc.Argv(rec.PID); err != nil || isOwnArgv(argv, rec) {
			return argv, err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return argv, nil
}

// isOwnArgv reports whether argv is rec's detached job: an `ask --job <id>` child or a split review child.
func isOwnArgv(argv []string, rec *Record) bool {
	return isJobArgv(argv, rec.ID) || isReviewArgv(argv, rec)
}

// isJobArgv reports whether argv carries the whole arguments `ask`, `--job` and id.
func isJobArgv(argv []string, id string) bool {
	return slices.Contains(argv, "ask") && slices.Contains(argv, "--job") && slices.Contains(argv, id)
}

// isReviewArgv reports whether argv is rec's detached split review: `<agent> review` at argv[1], or argv[2] under an interpreter.
func isReviewArgv(argv []string, rec *Record) bool {
	if rec.Progress == "" {
		return false
	}
	for _, i := range []int{1, 2} {
		if i+1 < len(argv) && argv[i] == rec.Agent && argv[i+1] == "review" {
			return true
		}
	}
	return false
}

// reloadRecord refreshes rec from disk, keeping rec as is when the load fails.
func reloadRecord(rec *Record) {
	if latest, err := LoadRecord(rec.ID); err == nil {
		*rec = latest
	}
}

// failIfRunning reloads rec and marks it Status "error" with msg only if it is still "running".
func failIfRunning(rec *Record, msg string) error {
	reloadRecord(rec)
	if rec.Status != "running" {
		return nil
	}
	rec.Status, rec.Error = "error", msg
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
		return failIfRunning(rec, "process gone (pid reused)")
	}
	argv, err := jobArgvSettled(rec)
	if err != nil {
		return fmt.Errorf("dispatch %q: cannot read pid %d command line: %w", rec.ID, rec.PID, err)
	}
	if !isOwnArgv(argv, rec) {
		if rec.Log == "" || (slices.Contains(argv, "ask") && !slices.Contains(argv, "--job")) { // a foreground ask
			return fmt.Errorf("dispatch %q is a foreground run; interrupt it in its own terminal", rec.ID)
		}
		return fmt.Errorf("dispatch %q: pid %d is a different program (pid reused); not signalling it", rec.ID, rec.PID)
	}
	if !sameStart(rec) {
		return fmt.Errorf("dispatch %q: pid %d is not the process spawned for it (pid reused); not signalling it", rec.ID, rec.PID)
	}
	if err := escalate(rec.PID, rec.ID); err != nil {
		return err
	}
	return failIfRunning(rec, fmt.Sprintf("cancelled by claude-code %s cancel", rec.Agent))
}

// sameStart reports whether rec.PID still has the start time recorded at spawn; no recorded time fails closed.
func sameStart(rec *Record) bool {
	if rec.PIDStart == "" {
		return false
	}
	got, err := proc.StartTime(rec.PID)
	return err == nil && got == rec.PIDStart
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

// Find resolves ref (exact id or unique prefix) across every agent's records, so the caller learns the agent.
func Find(ref string) (Record, error) {
	return Resolve(RecentRecords(1<<20), ref, func(Record) bool { return false })
}

// checkAgent errors when name is not a registered agent.
func checkAgent(name string) error {
	if _, ok := agent.LookupSchema(name); !ok {
		return fmt.Errorf("unknown agent %q", name)
	}
	return nil
}

// Row is one background-job line for `claude-code <v> status`.
type Row struct {
	ID       string
	Status   string // running | lost | ok | error (lost = running record, dead pid)
	Elapsed  string
	Preview  string
	Session  string
	Error    string `json:",omitempty"` // the failed dispatch's redacted error text
	Progress string `json:",omitempty"` // "part 2/3" while a split review runs
}

// Status lists rows for name's jobs in cwd's repo (all repos when all is set), newest first; ref narrows via Resolve.
func Status(ctx context.Context, name, cwd, ref string, all bool) ([]Row, error) {
	if err := checkAgent(name); err != nil {
		return nil, err
	}
	recs := List(name, RepoRoot(ctx, cwd), all || ref != "")
	if ref != "" {
		rec, err := Resolve(recs, ref, nil)
		if err != nil {
			return nil, err
		}
		recs = []Record{rec}
	}
	rows := make([]Row, len(recs))
	for i, r := range recs {
		rows[i] = toRow(r)
	}
	return rows, nil
}

// JobResult returns ref's record (default: newest finished) and its log text; a running match errors.
func JobResult(ctx context.Context, name, cwd, ref string) (Record, string, error) {
	if err := checkAgent(name); err != nil {
		return Record{}, "", err
	}
	recs := List(name, RepoRoot(ctx, cwd), ref != "") // an explicit id resolves across repos; the default stays local
	rec, err := Resolve(recs, ref, func(r Record) bool { return r.Status != "running" })
	if err != nil {
		return Record{}, "", err
	}
	if rec.Status == "running" {
		return Record{}, "", fmt.Errorf("job %s is still running", rec.ID)
	}
	log, _ := os.ReadFile(rec.Log) // best-effort: a missing log still returns the record
	return rec, string(log), nil
}

// CancelJob interrupts ref's job, default: the single running job for name in cwd's repo.
func CancelJob(ctx context.Context, name, cwd, ref string) (Record, error) {
	if err := checkAgent(name); err != nil {
		return Record{}, err
	}
	recs := List(name, RepoRoot(ctx, cwd), ref != "") // an explicit id resolves across repos; the default stays local
	rec, err := Resolve(recs, ref, func(r Record) bool { return r.Alive() })
	if err != nil {
		return Record{}, err
	}
	if !rec.Alive() {
		return Record{}, fmt.Errorf("job %s is not running", rec.ID)
	}
	if err := Cancel(&rec); err != nil {
		return Record{}, err
	}
	return rec, nil
}

// toRow derives a status Row from a dispatch record; redaction matches printDispatchLog (internal/agent/ask.go).
func toRow(r Record) Row {
	status := r.Status
	if status == "running" && !r.Alive() {
		status = "lost"
	}
	elapsed := r.Elapsed()
	if status == "running" || status == "lost" {
		elapsed = time.Since(r.Time).Round(time.Second).String()
	}
	preview := shapes.Preview(r.Request.Prompt, 60)
	return Row{ID: r.ID, Status: status, Elapsed: elapsed, Preview: preview, Session: r.SessionID,
		Error: shapes.Redact(r.Error), Progress: r.Progress}
}
