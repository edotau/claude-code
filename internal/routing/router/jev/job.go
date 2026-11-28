package jev

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
)

const (
	runScript = "run-finetune-jev.sh"
	trainerPy = "finetune_jev.py"
	lockWait  = 3 * time.Second
	tailBytes = 1 << 20 // how much of train.log's end the status and attach views read
	pollEvery = time.Second / 2
)

var (
	defaultPasses = []string{"tier,agent", "tier"}
	pythonRE      = regexp.MustCompile(`^python[23](\.[0-9]+)?$`)
	latestRE      = regexp.MustCompile(`\[train\] step|\[gold-gate\]|\[job |SERVE|NOTHING PASSED|Traceback|Error`)
	goldRE        = regexp.MustCompile(`\[gold-gate\]`)
	// ErrNoJob means no training job runs.
	ErrNoJob = errors.New("no job running; see: claude-code gateway jev --status")
)

// UsageError is a refusal the caller reports as a usage error.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

func usage(format string, a ...any) error { return &UsageError{Msg: fmt.Sprintf(format, a...)} }

// Job holds the trainer and output directories; its files share <Out>/job with the shell script.
type Job struct{ Trainer, Out string }

// StartOpts are the choices of a Start: Extra goes to the trainer and is kept across restarts.
type StartOpts struct {
	Fresh, Background   bool
	Labels, Tasks       string
	LabelsSet, TasksSet bool
	Extra               []string
}

// PassState is one pass of a job and whether its report is newer than the job's start.
type PassState struct {
	Pass string `json:"pass"`
	Done bool   `json:"done"`
}

// Status is the Jev server and the training job at one moment.
type Status struct {
	Server  string      `json:"server"` // up, down or off
	URL     string      `json:"url"`
	State   string      `json:"state"` // running, complete or not running
	PID     int         `json:"pid,omitempty"`
	Labels  string      `json:"labels"`
	Options string      `json:"options"`
	Passes  []PassState `json:"passes"`
	Latest  []string    `json:"latest"`
}

func (j Job) dir() string          { return filepath.Join(j.Out, "job") }
func (j Job) file(n string) string { return filepath.Join(j.dir(), n) }
func (j Job) script() string       { return filepath.Join(j.Trainer, runScript) }

func report(out, pass string) string {
	return filepath.Join(out, "report-"+strings.ReplaceAll(pass, ",", "-")+".json")
}

// readTrim is a file's text without trailing newlines ("" when unreadable), like the script's $(cat f).
func readTrim(p string) string {
	b, _ := os.ReadFile(p)
	return strings.TrimRight(string(b), "\r\n")
}

func writeFile(p string, b []byte) error { return paths.AtomicWrite(p, b, 0o644) }

func openLog(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
}

func (j Job) passes() []string {
	if t := readTrim(j.file("tasks")); t != "" {
		return []string{t}
	}
	return defaultPasses
}

func (j Job) passDone(pass string) bool {
	s, err := os.Stat(j.file("started"))
	if err != nil {
		return false
	}
	r, err := os.Stat(report(j.Out, pass))
	return err == nil && r.ModTime().After(s.ModTime())
}

func allDone(j Job) bool {
	return !slices.ContainsFunc(j.passes(), func(p string) bool { return !j.passDone(p) })
}

func optionArgs(j Job) []string {
	b, err := os.ReadFile(j.file("args"))
	if err != nil || len(b) == 0 {
		return nil
	}
	// The shell writes one argument per line, including empty arguments and spaces.
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func (j Job) optionsText() string {
	if a := optionArgs(j); len(a) > 0 {
		return strings.Join(a, " ")
	}
	return "trainer defaults"
}

// candidates only discovers candidates; every result must pass an executable/argv identity check.
func candidates(path string) []int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "pgrep", "-f", regexp.QuoteMeta(path)).Output()
	if err != nil {
		return nil
	}
	var ids []int
	for f := range strings.FieldsSeq(string(out)) {
		if n, e := strconv.Atoi(f); e == nil && n > 1 && n != os.Getpid() {
			ids = append(ids, n)
		}
	}
	return ids
}

// scriptArgs accepts only an executable position, never a path embedded in an option, echo or shell -c.
func scriptArgs(a []string) []string {
	if len(a) < 2 {
		return a
	}
	if filepath.Base(a[0]) == "caffeinate" && a[1] == "-is" {
		return scriptArgs(a[2:])
	}
	name := filepath.Base(a[0])
	if name == "bash" || name == "sh" || name == "zsh" || name == "python" ||
		pythonRE.MatchString(name) {
		return a[1:] // do not guess through interpreter flags such as -c or -m
	}
	return a
}

func optionValue(a []string, name string) string {
	value := ""
	for i, v := range a {
		if v == name && i+1 < len(a) {
			value = a[i+1]
		} else if after, ok := strings.CutPrefix(v, name+"="); ok {
			value = after
		}
	}
	return value
}

// sameExecutable also handles PATH invocations and symlinks used for a foreground run.
func sameExecutable(name string) bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	if name == exe {
		return true
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return false
	}
	a, err := os.Stat(path)
	b, other := os.Stat(exe)
	return err == nil && other == nil && os.SameFile(a, b)
}

// workerArgs recognizes the action before the trainer's -- separator, including Go boolean flag syntax.
func workerArgs(a []string) bool {
	if len(a) > 0 && a[0] == "--worker" {
		return true
	}
	train := false
	for i := 0; i < len(a) && a[i] != "--"; i++ {
		name, value, hasValue := strings.Cut(strings.TrimLeft(a[i], "-"), "=")
		enabled := true
		if hasValue {
			enabled, _ = strconv.ParseBool(value)
		}
		switch name {
		case "trainer", "out", "labels", "tasks", "format":
			if !hasValue {
				i++
			}
		case "train":
			train = enabled
		case "status", "attach", "stop", "options":
			if enabled {
				return false
			}
		}
	}
	return train
}

// role returns worker, legacy, wrapper or trainer only for a recognized executable and action.
func (j Job) role(pid int) string {
	a, err := proc.Argv(pid)
	if err != nil || len(a) == 0 {
		return ""
	}
	if len(a) > 3 && sameExecutable(a[0]) && a[1] == "gateway" && a[2] == "jev" {
		if workerArgs(a[3:]) {
			return "worker"
		}
		return ""
	}
	a = scriptArgs(a)
	if len(a) == 0 {
		return ""
	}
	if out := optionValue(a[1:], "--output"); out != "" && out != j.Out {
		return ""
	}
	switch a[0] {
	case filepath.Join(j.Trainer, "train-jev.sh"):
		if len(a) > 1 && (a[1] == "start" || a[1] == "run") {
			return "legacy"
		}
	case j.script():
		return "wrapper"
	case filepath.Join(j.Trainer, trainerPy):
		return "trainer"
	}
	return ""
}

func (j Job) recorded(pid int) bool {
	rec, start, ok := strings.Cut(readTrim(j.file("pid.start")), "\t")
	got, err := proc.StartTime(pid)
	return ok && rec == strconv.Itoa(pid) && start != "" && err == nil && got == start
}

// isJob fails closed on a stale start record; legacy pid files still require an exact executable role.
func (j Job) isJob(pid int) bool {
	if pid <= 1 || pid == os.Getpid() || !proc.Alive(pid) {
		return false
	}
	role := j.role(pid)
	if role == "" {
		return false
	}
	if rec, _, ok := strings.Cut(readTrim(j.file("pid.start")), "\t"); ok && rec == strconv.Itoa(pid) {
		return j.recorded(pid)
	}
	return role != "worker"
}

func (j Job) savedPID() int {
	pid, err := strconv.Atoi(strings.TrimSpace(readTrim(j.file("pid"))))
	if err != nil || !j.isJob(pid) {
		return 0
	}
	if !j.recorded(pid) && j.role(pid) != "legacy" {
		a, _ := proc.Argv(pid)
		if optionValue(a, "--output") != j.Out {
			return 0 // a legacy trainer without an output identity is not proof of this job
		}
	}
	return pid
}

// targets includes only verified job processes, not arbitrary pgrep substring matches.
func (j Job) targets() []int {
	var ids []int
	root := j.savedPID()
	if root > 0 {
		ids = append(ids, root)
		for _, pid := range proc.Descendants(root) {
			if j.isJob(pid) {
				ids = append(ids, pid)
			}
		}
	}
	for _, path := range []string{j.script(), filepath.Join(j.Trainer, trainerPy)} {
		for _, pid := range candidates(path) {
			a, _ := proc.Argv(pid)
			if j.isJob(pid) && optionValue(a, "--output") == j.Out {
				ids = append(ids, pid)
			}
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// PID conservatively treats a legacy trainer without --output as busy, but Stop needs stronger ownership proof.
func (j Job) PID() int {
	if pid := j.savedPID(); pid > 0 {
		return pid
	}
	for _, path := range []string{j.script(), filepath.Join(j.Trainer, trainerPy)} {
		for _, pid := range candidates(path) {
			if j.isJob(pid) {
				return pid
			}
		}
	}
	return 0
}

func (j Job) writePID(pid int) error {
	start, err := proc.StartTime(pid)
	if err != nil {
		return err // never publish an unauthenticated Go worker
	}
	if err := writeFile(j.file("pid.start"), fmt.Appendf(nil, "%d\t%s\n", pid, start)); err != nil {
		return err
	}
	return writeFile(j.file("pid"), fmt.Appendf(nil, "%d\n", pid))
}

// validate rejects values the shared, line-based shell format cannot represent.
func (o StartOpts) validate() error {
	if (o.LabelsSet && o.Labels == "") || (o.TasksSet && o.Tasks == "") {
		return usage("--labels and --tasks need nonempty values")
	}
	for _, v := range append([]string{o.Labels, o.Tasks}, o.Extra...) {
		if strings.ContainsAny(v, "\r\n\x00") {
			return usage("job options cannot contain newlines or NUL bytes")
		}
	}
	if strings.ContainsAny(o.Tasks, `/\\`) {
		return usage("--tasks must be a comma-separated task list, not a path")
	}
	return nil
}

func (o StartOpts) changesJob() bool {
	return o.TasksSet || o.LabelsSet || o.Tasks != "" || o.Labels != "" || len(o.Extra) > 0
}

// running is read-only, including refusals to change an active unfinished job's options.
func (j Job) running(o StartOpts, w io.Writer) (bool, error) {
	pid := j.PID()
	if pid == 0 {
		return false, nil
	}
	if !o.Fresh && o.changesJob() && !allDone(j) {
		return true, usage("the unfinished job keeps its options; stop it before using --fresh")
	}
	fmt.Fprintf(w, "already running (pid %d); see: claude-code gateway jev --status\n", pid)
	return true, nil
}

// Start begins a job, or continues the unfinished one: in this process, or detached with opts.Background.
func (j Job) Start(ctx context.Context, o StartOpts, w io.Writer) error {
	if err := o.validate(); err != nil {
		return err
	}
	if running, err := j.running(o, w); running {
		return err
	}
	if _, err := os.Stat(j.script()); err != nil {
		return usage("trainer script %s: %v", j.script(), err)
	}
	if err := os.MkdirAll(j.dir(), 0o755); err != nil {
		return err
	}
	foreground := false
	ran, err := paths.TryFileLock(j.file("lock"), lockWait, func() error {
		var e error
		foreground, e = j.begin(o, w)
		return e
	})
	if err != nil {
		return err
	}
	if !ran {
		return errors.New("another start holds the job lock; try again")
	}
	if !foreground {
		return nil
	}
	return j.foreground(ctx, w)
}

func (j Job) foreground(ctx context.Context, w io.Writer) error {
	defer j.clearPID()
	lf, err := openLog(j.file("train.log"))
	if err != nil {
		return err
	}
	defer lf.Close()
	fmt.Fprintf(w, "running in this terminal (pid %d); also logged to %s\n", os.Getpid(), j.file("train.log"))
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return j.Worker(ctx, io.MultiWriter(w, lf))
}

// begin runs under the lock: it prepares the job files and records the pid; foreground says the caller runs the worker.
func (j Job) begin(o StartOpts, w io.Writer) (foreground bool, err error) {
	if running, err := j.running(o, w); running {
		return false, err
	}
	if err := j.prepare(o, w); err != nil {
		return false, err
	}
	if o.Background {
		return false, j.spawn(w)
	}
	return true, j.writePID(os.Getpid())
}

// prepare starts a new job, or accepts continuing the unfinished one when no job option was given.
func (j Job) prepare(o StartOpts, w io.Writer) error {
	_, err := os.Stat(j.file("started"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if o.Fresh || errors.Is(err, os.ErrNotExist) || allDone(j) {
		return j.newJob(o, w)
	}
	if o.changesJob() {
		return usage("the unfinished job keeps its options (%s); add --fresh to start over with new ones", j.optionsText())
	}
	if err := j.checkInputs(); err != nil {
		return err
	}
	fmt.Fprintf(w, "continuing the unfinished job on labels %s; passes: %s; options: %s\n",
		readTrim(j.file("jury")), strings.Join(j.passes(), " "), j.optionsText())
	return nil
}

// newJob forgets the previous job and writes the new one's files; labels come from the flag, else the previous job.
func (j Job) newJob(o StartOpts, w io.Writer) error {
	labels := cmp.Or(o.Labels, readTrim(j.file("jury")))
	if labels == "" {
		return usage("a new job needs --labels DIR (the jury label directory)")
	}
	for _, p := range []string{j.file("started"), j.file("jury"), j.file("tasks"), j.file("args"),
		filepath.Join(j.Out, "resume.pt")} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	files := map[string]string{"jury": labels + "\n"}
	if o.Tasks != "" {
		files["tasks"] = o.Tasks + "\n"
	}
	if len(o.Extra) > 0 {
		files["args"] = strings.Join(o.Extra, "\n") + "\n"
	}
	for _, n := range []string{"jury", "tasks", "args"} {
		if files[n] == "" {
			continue
		}
		if err := writeFile(j.file(n), []byte(files[n])); err != nil {
			return err
		}
	}
	if err := writeFile(j.file("started"), nil); err != nil {
		return err
	}
	fmt.Fprintf(w, "new job on labels %s; passes: %s; options: %s\n", labels, strings.Join(j.passes(), " "), j.optionsText())
	return nil
}

// spawn detaches a worker that outlives this terminal and records its pid.
func (j Job) spawn(w io.Writer) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	lf, err := openLog(j.file("train.log"))
	if err != nil {
		return err
	}
	defer lf.Close()
	args := []string{"gateway", "jev", "--worker", "--trainer", j.Trainer, "--out", j.Out}
	pid := proc.SpawnDetachedPID(exe, args, "", lf)
	if pid == 0 {
		return errors.New("could not start the detached worker")
	}
	if err := j.writePID(pid); err != nil {
		return err // the child cannot train without its published pid and start record
	}
	fmt.Fprintf(w, "started (pid %d); log: %s\n", pid, j.file("train.log"))
	return nil
}

func jobf(w io.Writer, format string, a ...any) {
	fmt.Fprintf(w, "[job %s] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, a...))
}

// clearPID only removes our own record under the start/stop lock, never a successor's pid.
func (j Job) clearPID() {
	_, _ = paths.TryFileLock(j.file("lock"), lockWait, func() error {
		if readTrim(j.file("pid")) == strconv.Itoa(os.Getpid()) && j.recorded(os.Getpid()) {
			return errors.Join(os.Remove(j.file("pid")), os.Remove(j.file("pid.start")))
		}
		return nil
	})
}

// authorized waits for the launcher to finish publishing both pid files before any trainer may run.
func (j Job) authorized() error {
	ran, err := paths.TryFileLock(j.file("lock"), lockWait, func() error {
		if readTrim(j.file("pid")) != strconv.Itoa(os.Getpid()) || !j.recorded(os.Getpid()) {
			return errors.New("worker has no matching launch record; use --train")
		}
		return nil
	})
	if err == nil && !ran {
		return errors.New("launcher still holds the job lock")
	}
	return err
}

// checkInputs refuses damaged saved state rather than silently training with default labels or options.
func (j Job) checkInputs() error {
	for _, n := range []string{"jury", "tasks", "args"} {
		b, err := os.ReadFile(j.file(n))
		if n != "jury" && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if n == "jury" && len(strings.TrimSpace(string(b))) == 0 {
			return usage("unfinished job has no labels; use --fresh --labels DIR")
		}
	}
	return nil
}

// Worker runs the job's passes one after another, skipping finished ones; output goes to w.
func (j Job) Worker(ctx context.Context, w io.Writer) error {
	if err := j.authorized(); err != nil {
		return err
	}
	defer j.clearPID()
	if err := j.checkInputs(); err != nil {
		return err
	}
	labels, extra := readTrim(j.file("jury")), optionArgs(j)
	for _, p := range j.passes() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if j.passDone(p) {
			jobf(w, "pass %s already done, skipped", p)
			continue
		}
		jobf(w, "pass %s on %s; options: %s", p, labels, j.optionsText())
		_ = j.runPass(ctx, p, labels, extra, w) // a non-zero exit only means the gold gate failed
		if !j.passDone(p) {
			jobf(w, "pass %s did not finish; run: claude-code gateway jev --train", p)
			return fmt.Errorf("pass %s did not finish", p)
		}
	}
	jobf(w, "JOB COMPLETE")
	for _, l := range matchTail(j.file("train.log"), goldRE, 6) {
		fmt.Fprintln(w, l)
	}
	return nil
}

// runPass runs one trainer pass on the labels, keeping the machine awake when caffeinate exists.
func (j Job) runPass(ctx context.Context, pass, labels string, extra []string, w io.Writer) error {
	argv := append(slices.Clone(extra), "--tasks", pass, "--output", j.Out)
	name := j.script()
	if c, err := exec.LookPath("caffeinate"); err == nil {
		name, argv = c, append([]string{"-is", j.script()}, argv...)
	}
	cmd := exec.CommandContext(ctx, name, argv...)
	cmd.Env = append(os.Environ(), "JURY="+labels)
	cmd.Stdout, cmd.Stderr = w, w
	cmd.Cancel = func() error {
		_ = j.signalTargets()
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 15 * time.Second
	return cmd.Run()
}

// readTail returns the last tailBytes of the file and its full size.
func readTail(p string) ([]byte, int64) {
	f, err := os.Open(p)
	if err != nil {
		return nil, 0
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0
	}
	off := max(0, st.Size()-tailBytes)
	b := make([]byte, st.Size()-off)
	n, _ := f.ReadAt(b, off)
	return b[:n], st.Size()
}

// matchTail is the last n lines of the file's tail that match re.
func matchTail(p string, re *regexp.Regexp, n int) []string {
	b, _ := readTail(p)
	var hits []string
	for l := range strings.SplitSeq(string(b), "\n") {
		if re.MatchString(l) {
			hits = append(hits, l)
		}
	}
	return hits[max(0, len(hits)-n):]
}

// Status reads the Jev server and the job's files; it changes nothing.
func (j Job) Status(ctx context.Context) Status {
	st := Status{URL: cmp.Or(strings.TrimRight(proc.Value(ctx, urlEnv), "/"), defaultURL), Server: "up"}
	switch err := Probe(ctx); {
	case errors.Is(err, ErrOff):
		st.Server = "off"
	case err != nil:
		st.Server = "down"
	}
	st.PID = j.PID()
	switch {
	case st.PID > 0:
		st.State = "running"
	case allDone(j):
		st.State = "complete"
	default:
		st.State = "not running"
	}
	st.Labels, st.Options = cmp.Or(readTrim(j.file("jury")), "unknown"), j.optionsText()
	for _, p := range j.passes() {
		st.Passes = append(st.Passes, PassState{Pass: p, Done: j.passDone(p)})
	}
	st.Latest = matchTail(j.file("train.log"), latestRE, 8)
	return st
}

// Text prints the status the way the shell script did, with the server line first.
func (s Status) Text(w io.Writer) {
	fmt.Fprintf(w, "server: %s (%s)\n", s.Server, s.URL)
	switch s.State {
	case "running":
		fmt.Fprintf(w, "RUNNING (pid %d)\n", s.PID)
	case "complete":
		fmt.Fprintln(w, "COMPLETE")
	default:
		fmt.Fprintln(w, "NOT RUNNING, unfinished: claude-code gateway jev --train")
	}
	fmt.Fprintf(w, "labels: %s\noptions: %s\n", s.Labels, s.Options)
	for _, p := range s.Passes {
		fmt.Fprintf(w, "pass %s: %s\n", p.Pass, map[bool]string{true: "done", false: "pending"}[p.Done])
	}
	fmt.Fprintln(w, "--- latest (from train.log)")
	for _, l := range s.Latest {
		fmt.Fprintln(w, l)
	}
}

// Attach follows train.log from its last 40 lines until the job ends; ctx cancel detaches and never signals the job.
func (j Job) Attach(ctx context.Context, w io.Writer) error {
	pid := j.PID()
	if pid == 0 {
		return ErrNoJob
	}
	identity, err := proc.StartTime(pid)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "attached to pid %d (Ctrl-C detaches; the job keeps running)\n", pid)
	b, off := readTail(j.file("train.log"))
	lines := strings.SplitAfter(string(b), "\n")
	for _, l := range lines[max(0, len(lines)-41):] {
		fmt.Fprint(w, l)
	}
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for ended := false; !ended; {
		select {
		case <-ctx.Done():
			fmt.Fprintln(w, "\ndetached; the job is still running")
			return nil
		case <-tick.C:
		}
		start, err := proc.StartTime(pid)
		ended = err != nil || start != identity || !j.isJob(pid)
		off = follow(j.file("train.log"), w, off)
	}
	fmt.Fprintln(w, "--- job ended")
	j.Status(ctx).Text(w)
	return nil
}

// follow copies the log bytes after off to w and returns the new offset.
func follow(path string, w io.Writer, off int64) int64 {
	f, err := os.Open(path)
	if err != nil {
		return off
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return off
	}
	if st.Size() < off {
		off = 0 // truncated
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return off
	}
	n, _ := io.CopyN(w, f, st.Size()-off)
	return off + n
}

// signalTargets rechecks identity immediately before each signal and never signals this caller.
func (j Job) signalTargets() error {
	type target struct {
		pid   int
		start string
	}
	var targets []target
	ids := j.targets()
	if len(ids) == 0 && j.PID() != 0 {
		return errors.New("running process lacks safe output identity; refusing to signal")
	}
	for _, pid := range ids {
		if start, err := proc.StartTime(pid); err == nil {
			targets = append(targets, target{pid, start})
		}
	}
	var errs []error
	for _, t := range slices.Backward(targets) {
		p, err := os.FindProcess(t.pid)
		if err != nil {
			continue
		}
		start, err := proc.StartTime(t.pid)
		if err == nil && start == t.start && j.isJob(t.pid) {
			if err := p.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
				errs = append(errs, err)
			}
		}
		_ = p.Release()
	}
	return errors.Join(errs...)
}

// Stop serializes with launch and leaves pid records until the worker has actually exited.
func (j Job) Stop(w io.Writer) error {
	if _, err := os.Stat(j.dir()); errors.Is(err, os.ErrNotExist) {
		return ErrNoJob
	}
	ran, err := paths.TryFileLock(j.file("lock"), lockWait, j.signalTargets)
	if err != nil {
		return err
	}
	if !ran {
		return errors.New("another start/stop holds the job lock; try again")
	}
	_, err = fmt.Fprintln(w, "stop requested; continue with: claude-code gateway jev --train")
	return err
}

// Options runs the trainer's own --help.
func (j Job) Options(ctx context.Context, w io.Writer) error {
	cmd := exec.CommandContext(ctx, j.script(), "--help")
	cmd.Stdout, cmd.Stderr = w, os.Stderr
	return cmd.Run()
}

// remembered is the trainer and output directories kept after first use.
type remembered struct {
	Trainer string `json:"trainer"`
	Out     string `json:"out"`
}

func rememberedPath() string { return filepath.Join(paths.StateDir(), "jev", "job.json") }

// Resolve fills a missing trainer or out directory from the remembered pair and remembers any change.
func Resolve(trainer, out string) (Job, error) {
	var r remembered
	if b, err := os.ReadFile(rememberedPath()); err == nil {
		if json.Unmarshal(b, &r) != nil {
			r = remembered{} // never use a partially decoded corrupt pair
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Job{}, err
	}
	j := Job{Trainer: cmp.Or(trainer, r.Trainer), Out: cmp.Or(out, r.Out)}
	var missing []string
	for _, m := range []struct{ flag, v string }{{"--trainer", j.Trainer}, {"--out", j.Out}} {
		if m.v == "" {
			missing = append(missing, m.flag)
		}
	}
	if len(missing) > 0 {
		return Job{}, usage("nothing remembered yet: pass %s (kept after first use)", strings.Join(missing, " and "))
	}
	var err error
	if j.Trainer, err = filepath.Abs(j.Trainer); err != nil {
		return Job{}, err
	}
	if j.Out, err = filepath.Abs(j.Out); err != nil {
		return Job{}, err
	}
	if remembered(j) == r {
		return j, nil
	}
	b, err := json.Marshal(remembered(j))
	if err != nil {
		return Job{}, err
	}
	return j, paths.AtomicWrite(rememberedPath(), append(b, '\n'), 0o600)
}
