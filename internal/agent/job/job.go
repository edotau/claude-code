// job.go is background-job control on top of the dispatch log (claude-code ask --job).

package job

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/shapes"
)

// executable is swapped by tests.
var executable = os.Executable

// Spawn re-execs claude-code detached to run the saved record: argv ["ask","--job",rec.ID] + extra;
// stdout+stderr -> rec.Log (0600); sets rec.PID in memory for the caller's print, but never saves —
// the caller already saved rec as "running" before calling Spawn, and the --job child saves its own
// PID once it starts, so a post-Start save here could overwrite a fast child's finished status.
func Spawn(rec *agent.Record, extra []string) error {
	return SpawnArgv(rec, append([]string{"ask", "--job", rec.ID}, extra...), nil)
}

// SpawnArgv is Spawn for an arbitrary claude-code argv, with env appended to the child's environment.
func SpawnArgv(rec *agent.Record, argv, env []string) error {
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
	cmd.Env = append(os.Environ(), env...)
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}

	rec.PID, rec.Log = cmd.Process.Pid, logPath
	go func() { _ = cmd.Wait() }() // reap the detached child; its exit status is read from the record, not here
	return nil
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
	return agent.Record{}, errors.New("no matching dispatch found (see claude-code <agent> status)")
}

// Alive reports a running record whose PID is still alive.
func Alive(rec agent.Record) bool {
	return rec.Alive()
}

// jobArgvSettled reads pid's argv, retrying for ~500ms while it does not match: a child spawned a moment ago
// has forked but not yet exec'd, so /proc still shows the parent's argv.
func jobArgvSettled(pid int, id string) ([]string, error) {
	var argv []string
	var err error
	for range 5 {
		if argv, err = proc.Argv(pid); err != nil || isJobArgv(argv, id) {
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

// reloadRecord refreshes rec from disk, keeping rec as is when the load fails.
func reloadRecord(rec *agent.Record) {
	if latest, err := agent.LoadRecord(rec.ID); err == nil {
		*rec = latest
	}
}

// failIfRunning reloads rec and marks it Status "error" with msg only if it is still "running".
func failIfRunning(rec *agent.Record, msg string) error {
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
func Cancel(rec *agent.Record) error {
	if rec.PID <= 0 {
		return fmt.Errorf("dispatch %q has no pid to cancel", rec.ID)
	}
	if !proc.Alive(rec.PID) {
		return failIfRunning(rec, "process gone (pid reused)")
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
	return failIfRunning(rec, fmt.Sprintf("cancelled by claude-code %s cancel", rec.Agent))
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
func Find(ref string) (agent.Record, error) {
	return Resolve(agent.RecentRecords(1<<20), ref, func(agent.Record) bool { return false })
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
	recs := List(name, agent.RepoRoot(ctx, cwd), all || ref != "")
	if ref != "" {
		rec, err := Resolve(recs, ref, nil)
		if err != nil {
			return nil, err
		}
		recs = []agent.Record{rec}
	}
	rows := make([]Row, len(recs))
	for i, r := range recs {
		rows[i] = toRow(r)
	}
	return rows, nil
}

// JobResult returns ref's record (default: newest finished) and its log text; a running match errors.
func JobResult(ctx context.Context, name, cwd, ref string) (agent.Record, string, error) {
	if err := checkAgent(name); err != nil {
		return agent.Record{}, "", err
	}
	recs := List(name, agent.RepoRoot(ctx, cwd), ref != "") // an explicit id resolves across repos; the default stays local
	rec, err := Resolve(recs, ref, func(r agent.Record) bool { return r.Status != "running" })
	if err != nil {
		return agent.Record{}, "", err
	}
	if rec.Status == "running" {
		return agent.Record{}, "", fmt.Errorf("job %s is still running", rec.ID)
	}
	log, _ := os.ReadFile(rec.Log) // best-effort: a missing log still returns the record
	return rec, string(log), nil
}

// tailBeat is the silence before a follow prints its heartbeat; tailPoll is the log re-read interval. Tests swap both.
var (
	tailBeat = 15 * time.Second
	tailPoll = 250 * time.Millisecond
)

// Tail copies ref's log (default: name's newest job in cwd's repo) to w. With follow it keeps copying until the
// record stops being alive, printing a heartbeat line per tailBeat of silence. It returns the record as last seen.
func Tail(ctx context.Context, name, cwd, ref string, follow bool, w io.Writer) (agent.Record, error) {
	if err := checkAgent(name); err != nil {
		return agent.Record{}, err
	}
	recs := List(name, agent.RepoRoot(ctx, cwd), ref != "") // an explicit id resolves across repos; the default stays local
	rec, err := Resolve(recs, ref, nil)
	if err != nil {
		return agent.Record{}, err
	}
	f, err := os.Open(rec.LogPath())
	for follow && errors.Is(err, os.ErrNotExist) && Alive(rec) { // the CLI child may not have started yet
		if err = sleepCtx(ctx, tailPoll); err != nil {
			return rec, err
		}
		rec = reloaded(rec)
		f, err = os.Open(rec.LogPath())
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rec, fmt.Errorf("job %s has no log (it never started the CLI, or predates dispatch logs)", rec.ID)
		}
		return rec, err
	}
	defer f.Close()
	if _, err := io.Copy(w, f); err != nil || !follow {
		return reloaded(rec), err
	}
	lastOut, lastBeat := time.Now(), time.Time{}
	for Alive(rec) {
		if err := sleepCtx(ctx, tailPoll); err != nil {
			return rec, err
		}
		rec = reloaded(rec)
		n, err := io.Copy(w, f)
		if err != nil {
			return rec, err
		}
		now := time.Now()
		if n > 0 {
			lastOut = now
		} else if quiet := now.Sub(lastOut); now.Sub(lastBeat) >= tailBeat && quiet >= tailBeat {
			lastBeat = now
			fmt.Fprintf(w, "· no output for %s (elapsed %s)\n", quiet.Round(time.Second), now.Sub(rec.Time).Round(time.Second))
		}
	}
	_, err = io.Copy(w, f) // whatever landed between the last read and the record leaving running
	return rec, err
}

// TailEvents copies rec's <id>.events.jsonl to w; with follow it waits for a result event or until the record
// leaves running, printing a heartbeat event per tailBeat of silence. It returns the record as last seen.
func TailEvents(ctx context.Context, rec agent.Record, follow bool, w io.Writer) (agent.Record, error) {
	f, err := os.Open(rec.EventsPath())
	for follow && errors.Is(err, os.ErrNotExist) && Alive(rec) { // the job may not have started yet
		if err = sleepCtx(ctx, tailPoll); err != nil {
			return rec, err
		}
		rec = reloaded(rec)
		f, err = os.Open(rec.EventsPath())
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rec, fmt.Errorf("job %s has no event stream (it predates event logs, or never started)", rec.ID)
		}
		return rec, err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	var pend []byte
	lastOut, lastBeat := time.Now(), time.Time{}
	for {
		n, done, err := copyEventLines(br, &pend, w)
		if err != nil || done || !follow {
			return reloaded(rec), err
		}
		if n > 0 {
			lastOut = time.Now()
		}
		if !Alive(rec) { // the writer is gone: one last drain, then stop
			_, _, err = copyEventLines(br, &pend, w)
			return rec, err
		}
		if err := sleepCtx(ctx, tailPoll); err != nil {
			return rec, err
		}
		rec = reloaded(rec)
		if now := time.Now(); n == 0 && now.Sub(lastBeat) >= tailBeat && now.Sub(lastOut) >= tailBeat {
			lastBeat = now
			fmt.Fprintf(w, "{\"schema\":%d,\"dispatch_id\":%q,\"type\":\"heartbeat\",\"t_ms\":%d}\n",
				agent.EventSchema, rec.ID, now.Sub(rec.Time).Milliseconds())
		}
	}
}

// copyEventLines writes each complete line br has to w, holding a partial tail in pend; done = a result line passed.
func copyEventLines(br *bufio.Reader, pend *[]byte, w io.Writer) (n int, done bool, err error) {
	for {
		chunk, rerr := br.ReadBytes('\n')
		*pend = append(*pend, chunk...)
		if rerr != nil { // EOF: the rest of the line has not landed yet
			return n, done, nil
		}
		if _, err := w.Write(*pend); err != nil {
			return n, done, err
		}
		var ev struct{ Type string }
		if json.Unmarshal(*pend, &ev) == nil && ev.Type == agent.EventResult {
			done = true
		}
		n += len(*pend)
		*pend = (*pend)[:0]
		if done {
			return n, true, nil
		}
	}
}

// reloaded re-reads rec from the dispatch log; a record pruned mid-follow keeps its last known state.
func reloaded(rec agent.Record) agent.Record {
	if latest, err := agent.LoadRecord(rec.ID); err == nil {
		return latest
	}
	return rec
}

// sleepCtx waits d, returning ctx.Err() if ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// CancelJob interrupts ref's job, default: the single running job for name in cwd's repo.
func CancelJob(ctx context.Context, name, cwd, ref string) (agent.Record, error) {
	if err := checkAgent(name); err != nil {
		return agent.Record{}, err
	}
	recs := List(name, agent.RepoRoot(ctx, cwd), ref != "") // an explicit id resolves across repos; the default stays local
	rec, err := Resolve(recs, ref, func(r agent.Record) bool { return Alive(r) })
	if err != nil {
		return agent.Record{}, err
	}
	if !Alive(rec) {
		return agent.Record{}, fmt.Errorf("job %s is not running", rec.ID)
	}
	if err := Cancel(&rec); err != nil {
		return agent.Record{}, err
	}
	return rec, nil
}

// toRow derives a status Row from a dispatch record; redaction matches printDispatchLog (internal/cli/agent.go).
func toRow(r agent.Record) Row {
	status := r.Status
	if status == "running" && !Alive(r) {
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
