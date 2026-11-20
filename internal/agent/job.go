// job.go is background-job control on top of the dispatch log (claude-code agents ask --job).

package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/textutil"
)

// RepoRoot is the main checkout root for cwd, falling back to cwd when git fails.
func RepoRoot(ctx context.Context, cwd string) string {
	_, repo := treeAndRepo(ctx, cwd)
	return repo
}

// WritableRoots is where a tools dispatch may write besides its cwd: the harness checkout and cwd's repo.
func WritableRoots(cwd string) []string {
	roots := []string{paths.Real(paths.ConfigDir())}
	if cwd == "" {
		return roots
	}
	if repo := paths.Real(RepoRoot(context.Background(), cwd)); repo != "" && repo != roots[0] {
		roots = append(roots, repo)
	}
	return roots
}

// treeAndRepo is cwd's git top (the working tree) and its main checkout root; "" and cwd when git fails.
func treeAndRepo(ctx context.Context, cwd string) (tree, repo string) {
	top := paths.GitTop(ctx, cwd)
	if top == "" {
		return "", cwd
	}
	return top, paths.MainCheckout(top)
}

type treeKey struct{}

// startTreeAndRepo runs treeAndRepo for workdir ("" is the process cwd) in the background; the func joins it.
func startTreeAndRepo(ctx context.Context, workdir string) func() (string, string) {
	done := make(chan [2]string, 1)
	go func() {
		cwd := workdir
		if cwd == "" {
			cwd, _ = os.Getwd()
		}
		t, r := treeAndRepo(ctx, cwd)
		done <- [2]string{t, r}
	}()
	return sync.OnceValues(func() (string, string) { p := <-done; return p[0], p[1] })
}

// executable is swapped by tests.
var executable = os.Executable

// SpawnContext re-execs claude-code detached on rec; it never saves, as a post-Start save could clobber a fast child.
func SpawnContext(ctx context.Context, rec *Record, extra []string) error {
	return SpawnArgvContext(ctx, rec, append([]string{"agents", "ask", "--job", rec.ID}, extra...), nil)
}

// SpawnArgvContext preserves run values while detaching the child lifetime.
func SpawnArgvContext(ctx context.Context, rec *Record, argv, env []string) error {
	ctx, _ = RunValues(ctx, nil)
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
	cmd.Env = (EnvSpec{Mode: EnvClaude, Extra: env}).Environ(context.WithoutCancel(ctx))
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}

	rec.PID, rec.Log, rec.Background = cmd.Process.Pid, logPath, true
	rec.PIDStart, _ = proc.StartTime(rec.PID) // "" on failure: Cancel then refuses to signal this pid
	go func() { _ = cmd.Wait() }()            // reap the detached child; its exit status is read from the record, not here
	return nil
}

// list is name's dispatch records, newest first; a ref decodes only the files it prefixes, else !all keeps Repo.
func list(ctx context.Context, name, cwd, ref string, all bool) []Record {
	repo := ""
	if ref == "" && !all {
		repo = RepoRoot(ctx, cwd)
	}
	var out []Record
	names, _ := recordNames(paths.DispatchDir())
	for _, n := range slices.Backward(names) {
		if !strings.HasPrefix(n, ref) {
			continue
		}
		if r, err := readRecord(filepath.Join(paths.DispatchDir(), n)); err == nil && r.Agent == name && (ref != "" || all || r.Repo == repo) {
			out = append(out, r)
		}
	}
	return out
}

// resolveRecord picks by exact id, unique id prefix, else the newest record matching pred.
func resolveRecord(recs []Record, ref string, pred func(Record) bool) (Record, error) {
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
			return Record{}, fmt.Errorf("no dispatch matches %q (see claude-code agents status)", ref)
		default:
			return Record{}, fmt.Errorf("%q matches %d dispatches, be more specific (see claude-code agents status)", ref, len(match))
		}
	}
	for _, r := range recs {
		if pred == nil || pred(r) {
			return r, nil
		}
	}
	return Record{}, errors.New("no matching dispatch found (see claude-code agents status)")
}

// jobArgvSettled reads rec's pid argv, retrying ~500ms: a forked, not yet exec'd child shows the parent's /proc argv.
func jobArgvSettled(rec *Record) ([]string, error) {
	var argv []string
	var err error
	for i := range 5 {
		if argv, err = proc.Argv(rec.PID); err != nil || isOwnArgv(argv, rec) {
			return argv, err
		}
		if i < 4 {
			time.Sleep(pollStep)
		}
	}
	return argv, nil
}

// isOwnArgv reports whether argv is rec's detached job: an `ask --job <id>` child or a split review child.
func isOwnArgv(argv []string, rec *Record) bool {
	return isJobArgv(argv, rec.ID) || isReviewArgv(argv, rec)
}

// isJobArgv reports whether argv carries the whole arguments `ask` or `dispatch --waves`, `--job` and id.
func isJobArgv(argv []string, id string) bool {
	runner := slices.Contains(argv, "ask") || (slices.Contains(argv, "dispatch") && slices.Contains(argv, "--waves"))
	return runner && slices.Contains(argv, "--job") && slices.Contains(argv, id)
}

// isReviewArgv reports whether argv is rec's split review: `<agent> review` at argv[1], or [2] under an interpreter.
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

func failIfRunning(rec *Record, msg string, deadOnly bool) (bool, error) {
	fix := func(r Record) Record {
		r.Status, r.FailKind = "error", FailAvailability // a cancel or a dead process says nothing about the model
		if r.Error == "" {
			r.Error = msg
		}
		return r
	}
	update := UpdateRunning
	if deadOnly {
		update = RepairDead
	}
	cur, applied, err := update(rec.ID, fix)
	if err != nil {
		return false, err
	}
	*rec = cur
	return applied, nil
}

// Cancel checks rec.PID is still its job, SIGINTs it (SIGKILL after 5s), then marks a still-running record cancelled.
func Cancel(rec *Record) error {
	_, err := cancel(rec)
	return err
}

func cancel(rec *Record) (bool, error) {
	if rec.PID <= 0 {
		return false, fmt.Errorf("dispatch %q has no pid to cancel", rec.ID)
	}
	if !proc.Alive(rec.PID) {
		return failIfRunning(rec, "process gone", true)
	}
	if err := checkJobPID(rec); err != nil {
		return false, err
	}
	if err := escalate(rec.PID, rec.ID); err != nil {
		return false, err
	}
	return failIfRunning(rec, "cancelled by claude-code agents cancel", false)
}

// checkJobPID refuses to signal a rec.PID that is a foreground run or a reused pid.
func checkJobPID(rec *Record) error {
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
	return nil
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

// pollStep paces waitGone and jobArgvSettled; a var so tests can shorten it.
var pollStep = 100 * time.Millisecond

// procTable is the process table escalate reads to see group members outlive their leader; tests swap it.
var procTable = proc.List

// escalate sends SIGINT, waits cancelGrace, then SIGKILL and waits killWait more; errors if pid's group outlives both.
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

// waitGone polls up to d for pid and every other member of its process group to be gone.
func waitGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if groupGone(pid) {
			return true
		}
		time.Sleep(pollStep)
	}
	return groupGone(pid)
}

// groupGone reports pid dead with no process left in group pid; an unreadable table counts as not gone.
func groupGone(pid int) bool {
	if proc.Alive(pid) {
		return false
	}
	procs, err := procTable(proc.DispatchIDEnv)
	if err != nil {
		return false
	}
	return !slices.ContainsFunc(procs, func(p proc.Info) bool { return p.PGID == pid })
}

// wavesJobAgent is the job-only agent name of a detached `dispatch --waves` runner; it is not a launchable agent.
const wavesJobAgent = "waves"

// checkAgent errors when name is neither a registered agent nor the job-only waves runner.
func checkAgent(name string) error {
	if _, ok := LookupSchema(name); !ok && name != wavesJobAgent {
		return fmt.Errorf("unknown agent %q", name)
	}
	return nil
}

// Row is one background-job line for `claude-code agents status`.
type Row struct {
	ID       string
	Status   string // running | lost | ok | error | partial | stalled | quota (lost = running record, dead pid)
	Elapsed  string
	Preview  string
	Session  string
	Error    string `json:",omitempty"`     // the failed dispatch's redacted error text
	Progress string `json:",omitempty"`     // "part 2/3" while a split review runs
	Cutoff   string `json:",omitempty"`     // how a cut-off run ended: deadline, stall, idle, cancelled, auth-retry
	Live     string `json:"live,omitempty"` // a running row's progress line

	Parent string      `json:"parent,omitempty"`
	Depth  int         `json:"depth,omitempty"`
	Usage  *wire.Usage `json:"usage,omitempty"`
}

// Observed is what the harness itself recorded about a dispatch, apart from the agent's own account.
type Observed struct {
	Parent   string      `json:"parent,omitempty"`
	Depth    int         `json:"depth,omitempty"`
	Usage    *wire.Usage `json:"usage,omitempty"`
	Model    string      `json:"model,omitempty"`
	Route    string      `json:"route,omitempty"`
	Duration string      `json:"duration,omitempty"`
	State    string      `json:"state,omitempty"`
	Files    *int        `json:"files,omitempty"` // files the run changed; nil: not observed
}

// ObservedOf is rec's harness-observed facts, taken from the record alone.
func ObservedOf(rec Record) *Observed {
	o := &Observed{Parent: rec.Parent, Depth: rec.Depth, Usage: rec.Usage, Model: cmp.Or(rec.Model, rec.Invoked),
		Route: rec.Route, State: rec.Status}
	if rec.DurationMs > 0 {
		o.Duration = roundedRun(rec)
	}
	if n, _ := wroteCount(rec); rec.Wrote != nil {
		o.Files = &n
	}
	return o
}

// roundedRun is rec's recorded run time to the second.
func roundedRun(rec Record) string {
	return (time.Duration(rec.DurationMs) * time.Millisecond).Round(time.Second).String()
}

// wroteCount is how many files rec.Wrote names and whether the list was cut at its cap.
func wroteCount(rec Record) (int, bool) {
	if rec.Wrote == nil {
		return 0, false
	}
	n := len(*rec.Wrote)
	if n > 0 && (*rec.Wrote)[n-1] == WroteTruncated {
		return n - 1, true
	}
	return n, false
}

// ObservedLine is the one-line `observed:` footer ("3 files · 41k tok · depth 1 · 2m10s"), or "" for a legacy record.
func ObservedLine(rec Record) string {
	var parts []string
	if n, more := wroteCount(rec); rec.Wrote != nil {
		plus := ""
		if more {
			plus = "+"
		}
		parts = append(parts, fmt.Sprintf("%d%s files", n, plus))
	} else if rec.TreeChanged != nil {
		parts = append(parts, fmt.Sprintf("%d files changed in a shared tree", len(*rec.TreeChanged)))
	}
	if u := rec.Usage; u != nil {
		if t := int64(u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens + u.OutputTokens); t > 0 {
			parts = append(parts, textutil.CompactCount(t)+" tok")
		}
	}
	if rec.Depth > 0 {
		parts = append(parts, fmt.Sprintf("depth %d", rec.Depth))
	}
	if len(parts) == 0 {
		return ""
	}
	if rec.DurationMs > 0 {
		parts = append(parts, roundedRun(rec))
	}
	return textutil.Truncate("observed: "+strings.Join(parts, " · "), 80)
}

// Detail is a status row's tail: progress and preview, or the preview plus the one-line error text.
func (r Row) Detail() string {
	d := cmp.Or(r.Live, r.Preview)
	if r.Cutoff != "" {
		d = "(" + r.Cutoff + ") " + d
	}
	if r.Progress != "" {
		d = "[" + r.Progress + "] " + d
	}
	if r.Error != "" {
		d += " -- " + textutil.PreviewRedacted(r.Error, 300)
	}
	return d
}

// Status lists rows for name's jobs in cwd's repo (all repos when all is set), newest first; ref narrows via Resolve.
func Status(ctx context.Context, name, cwd, ref string, all bool) ([]Row, error) {
	if err := checkAgent(name); err != nil {
		return nil, err
	}
	recs := list(ctx, name, cwd, ref, all)
	if ref != "" {
		rec, err := resolveRecord(recs, ref, nil)
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

// resolveJob is resolveRecord over name's jobs; an explicit ref resolves across repos, the default stays local.
func resolveJob(ctx context.Context, name, cwd, ref string, pred func(Record) bool) (Record, error) {
	if err := checkAgent(name); err != nil {
		return Record{}, err
	}
	return resolveRecord(list(ctx, name, cwd, ref, false), ref, pred)
}

// ResolveJob is name's job ref (default: its newest job in cwd's repo), running or finished.
func ResolveJob(ctx context.Context, name, cwd, ref string) (Record, error) {
	return resolveJob(ctx, name, cwd, ref, nil)
}

// JobResult returns ref's record (default: newest finished) and its log text; a running match errors.
func JobResult(ctx context.Context, name, cwd, ref string) (Record, string, error) {
	rec, err := resolveJob(ctx, name, cwd, ref, func(r Record) bool { return r.Status != "running" })
	if err != nil {
		return Record{}, "", err
	}
	if rec.Status == "running" {
		return Record{}, "", fmt.Errorf("job %s is still running", rec.ID)
	}
	log, _ := os.ReadFile(rec.Log) // best-effort: a missing log still returns the record
	return rec, string(log), nil
}

type CancelResult struct {
	Record
	AlreadyFinished bool
}

func CancelJob(ctx context.Context, name, cwd, ref string) (CancelResult, error) {
	rec, err := resolveJob(ctx, name, cwd, ref, func(r Record) bool { return r.Alive() })
	if err != nil {
		return CancelResult{}, err
	}
	if !rec.Alive() {
		return CancelResult{}, fmt.Errorf("job %s is not running", rec.ID)
	}
	applied, err := cancel(&rec)
	if err != nil {
		return CancelResult{}, err
	}
	if rec.Status == "running" {
		return CancelResult{}, fmt.Errorf("job %s is still running", rec.ID)
	}
	return CancelResult{Record: rec, AlreadyFinished: !applied}, nil
}

// toRow derives a status Row from a dispatch record; redaction matches Record.LogLine.
func toRow(r Record) Row {
	status := r.Status
	if status == "running" && !r.Alive() {
		status = "lost"
	}
	elapsed := r.Elapsed()
	if status == "running" || status == "lost" {
		elapsed = time.Since(r.Time).Round(time.Second).String()
	}
	preview := textutil.PreviewRedacted(r.Request.Prompt, 60)
	live := ""
	if status == "running" {
		live = ProgressOf(r, time.Now()).Line(progressWidth)
	}
	return Row{Live: live, ID: r.ID, Status: status, Elapsed: elapsed, Preview: preview, Session: r.SessionID,
		Error: textutil.RedactSecrets(r.Error), Progress: r.Progress, Cutoff: r.Cutoff,
		Parent: r.Parent, Depth: r.Depth, Usage: r.Usage}
}

// interrupt sends SIGINT to pid's process group and pid.
func interrupt(pid int) error { return proc.SignalGroup(pid, syscall.SIGINT) }

// kill sends SIGKILL to pid's process group and pid; the caller verifies termination by polling.
func kill(pid int) error { return proc.SignalGroup(pid, syscall.SIGKILL) }
