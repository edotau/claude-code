package memory

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

const (
	queueSchema      = 1
	queueMaxAttempts = 3
)

var queueKinds = []string{"update", "watch", "dream", "catchup", "embed", "learn-routing", "learn-recall"}

// Job is one queued background task; Kind+Key name its file, so a duplicate enqueue coalesces.
type Job struct {
	Schema    int               `json:"schema"`
	Kind      string            `json:"kind"`
	Key       string            `json:"key"`
	Args      []string          `json:"args,omitempty"`
	CWD       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Enqueued  time.Time         `json:"enqueued"`
	NotBefore time.Time         `json:"not_before,omitzero"`
	Attempts  int               `json:"attempts"`
}

func queueDir() string { return filepath.Join(paths.MemoryStateDir(), "queue") }

// jobName keeps a bounded readable prefix and hashes the original key to distinguish sanitized and case variants.
func jobName(kind, key string) string {
	clean := strings.Map(func(r rune) rune {
		if r == '.' || r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return '_'
	}, key)
	return fmt.Sprintf("%s-%.32s-%x.json", kind, clean, sha256.Sum256([]byte(key)))
}

// Enqueue files j unless a job with the same kind and key waits; queued=false means coalesced.
func Enqueue(j Job) (queued bool, err error) {
	if !slices.Contains(queueKinds, j.Kind) || j.Key == "" {
		return false, fmt.Errorf("memory queue: bad job kind %q key %q", j.Kind, j.Key)
	}
	j.Schema = queueSchema
	if j.Enqueued.IsZero() {
		j.Enqueued = time.Now().UTC()
	}
	return queueAdd(queueDir(), j)
}

// queueAdd writes a temp file then hard-links it to the final name, so the name appears whole or not at all.
func queueAdd(dir string, j Job) (bool, error) {
	b, err := json.Marshal(j)
	if err != nil {
		return false, err
	}
	final := filepath.Join(dir, jobName(j.Kind, j.Key))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	f, err := os.CreateTemp(dir, ".enqueue-*")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	_, writeErr := f.Write(b)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return false, err
	}
	if !j.NotBefore.IsZero() { // the mtime is the due time, so queuedDue never reads a waiting job
		if err := os.Chtimes(tmp, j.NotBefore, j.NotBefore); err != nil {
			return false, err
		}
	}
	if err := os.Link(tmp, final); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func readJob(path string) (Job, bool) {
	var j Job
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &j) != nil || j.Kind == "" {
		return Job{}, false
	}
	return j, true
}

// queued lists the waiting job files (dot-prefixed temp files excluded).
func queued() []string {
	m, _ := filepath.Glob(filepath.Join(queueDir(), "[a-z]*.json"))
	return m
}

func pending() int { return len(queued()) }

func queuedDue(now time.Time) bool {
	for _, path := range queued() {
		if fi, err := os.Stat(path); err != nil || fi.ModTime().After(now) {
			continue
		}
		if j, ok := readJob(path); ok && !j.NotBefore.After(now) {
			return true
		}
	}
	return false
}

// claimNext moves the oldest due job into running/ and returns it with its running path.
func claimNext(now time.Time) (Job, string, bool) {
	type cand struct {
		j    Job
		path string
	}
	var due []cand
	for _, p := range queued() {
		if j, ok := readJob(p); ok && !j.NotBefore.After(now) {
			due = append(due, cand{j, p})
		}
	}
	slices.SortFunc(due, func(a, b cand) int { return a.j.Enqueued.Compare(b.j.Enqueued) })
	runDir := filepath.Join(queueDir(), "running")
	if os.MkdirAll(runDir, 0o700) != nil {
		return Job{}, "", false
	}
	start, err := proc.StartTime(os.Getpid())
	if err != nil {
		return Job{}, "", false
	}
	for _, c := range due {
		dst := filepath.Join(runDir, filepath.Base(c.path)+fmt.Sprintf(".%d.%x.%s", os.Getpid(),
			sha256.Sum256([]byte(start)), rand.Text()))
		if os.Rename(c.path, dst) == nil {
			if j, ok := claimedJob(dst, now); ok {
				return j, dst, true
			}
		}
	}
	return Job{}, "", false
}

func claimedJob(dst string, now time.Time) (Job, bool) {
	j, ok := readJob(dst)
	if !ok {
		failJob(dst)
		return Job{}, false
	}
	if j.NotBefore.After(now) {
		if _, err := queueAdd(queueDir(), j); err == nil {
			_ = finish(dst)
		}
		return Job{}, false
	}
	return j, true
}

// finish drops a completed running job, then its child identity: a death between them leaves only a stale identity.
func finish(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(childPath(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func childPath(path string) string {
	return filepath.Join(queueDir(), "children", filepath.Base(path))
}

func childAlive(path string) bool {
	data, err := os.ReadFile(childPath(path))
	if err != nil {
		return false
	}
	pidText, token, ok := strings.Cut(strings.TrimSpace(string(data)), " ")
	pid, err := strconv.Atoi(pidText)
	if ok && err == nil && pid == 0 { // pre-spawn lease: the child may exist until the job's own deadline
		sec, _ := strconv.ParseInt(token, 10, 64)
		return time.Now().Before(time.Unix(sec, 0))
	}
	if !ok || err != nil || pid < 0 {
		return true // An unreadable identity must not permit a concurrent replay.
	}
	start, err := proc.StartTime(pid)
	return err != nil && !errors.Is(err, proc.ErrNoProcess) || err == nil && start == token
}

// requeueDead returns running jobs whose worker died to the queue with attempts+1; the third attempt goes to failed/.
// A job whose child still runs stays in running/; the rest of the queue drains regardless.
func requeueDead() {
	_, _ = paths.TryFileLock(filepath.Join(queueDir(), "recovery.lock"), 0, func() error {
		runs, _ := filepath.Glob(filepath.Join(queueDir(), "running", "*.json.*"))
		for _, p := range runs {
			if !queueOwnerDead(p) {
				continue
			}
			if childAlive(p) {
				continue
			}
			j, ok := readJob(p)
			j.Attempts++
			if !ok || j.Attempts >= queueMaxAttempts {
				failJob(p)
				continue
			}
			if _, err := queueAdd(queueDir(), j); err == nil {
				if err := finish(p); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// runQueuedJob is the subprocess boundary; tests never execute their own binary.
var runQueuedJob = func(ctx context.Context, self string, j Job, path string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, self, j.Args...)
	cmd.Dir = j.CWD
	cmd.Stdout, cmd.Stderr = out, out
	proc.OwnGroup(cmd) // the timeout kills the job's whole group, so its claude child cannot outlive the lock
	cmd.WaitDelay = proc.ExecWaitDelay
	deadline, _ := ctx.Deadline()
	if err := os.MkdirAll(filepath.Dir(childPath(path)), 0o700); err != nil {
		return err
	}
	if err := paths.AtomicWrite(childPath(path), []byte(fmt.Sprintf("0 %d\n", deadline.Unix())), 0o600); err != nil {
		return err
	}
	gate, closeGate, err := gateChild(cmd)
	if err != nil {
		return err
	}
	defer closeGate() // a worker killed before the release closes the gate empty, so the child exits unrun
	if err := cmd.Start(); err != nil {
		return err
	}
	start, err := proc.StartTime(cmd.Process.Pid)
	if err == nil {
		err = paths.AtomicWrite(childPath(path), []byte(fmt.Sprintf("%d %s\n", cmd.Process.Pid, start)), 0o600)
	}
	if err == nil && gate != nil {
		_, err = gate.Write([]byte{'1'})
	}
	if err != nil {
		if cmd.Cancel != nil {
			_ = cmd.Cancel()
		} else {
			_ = cmd.Process.Kill()
		}
		return errors.Join(err, cmd.Wait())
	}
	return cmd.Wait()
}

// queueGateEnv tells a queued child to wait on fd 3 for the parent's go byte; no byte before EOF means exit.
const queueGateEnv = "HARNESS_QUEUE_GATE"

// gateChild wires cmd's fd 3 to a pipe the parent writes after recording the child; closeGate drops both parent ends.
func gateChild(cmd *exec.Cmd) (w *os.File, closeGate func(), err error) {
	if runtime.GOOS == "windows" { // ExtraFiles is unsupported there
		return nil, func() {}, nil
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.ExtraFiles = []*os.File{r}
	cmd.Env = append(os.Environ(), queueGateEnv+"=1")
	return w, func() { _ = r.Close(); _ = w.Close() }, nil
}

// awaitGate reports whether the parent released the child; the env is cleared so grandchildren do not wait.
func awaitGate(r io.Reader) bool {
	_ = proc.Unsetenv(queueGateEnv)
	var b [1]byte
	n, _ := io.ReadFull(r, b[:])
	return n == 1
}

func init() {
	if os.Getenv(queueGateEnv) == "" {
		return
	}
	f := os.NewFile(3, "queue-gate")
	if !awaitGate(f) {
		os.Exit(0)
	}
	_ = f.Close()
}

// drainQueue runs every due job serially; a failed job is finished, never retried.
func drainQueue(out io.Writer) {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(out, "memory queue:", err)
		return
	}
	requeueDead()
	for {
		j, path, ok := claimNext(time.Now())
		if !ok {
			return
		}
		fmt.Fprintf(out, "queue %s %s\n", j.Kind, j.Key)
		ctx, cancel := context.WithTimeout(context.Background(), updateTimeout+2*time.Minute)
		if err := runQueuedJob(ctx, self, j, path, out); err != nil {
			fmt.Fprintf(out, "queue %s %s: %v\n", j.Kind, j.Key, err)
		}
		cancel()
		if err := finish(path); err != nil {
			fmt.Fprintf(out, "queue %s %s: %v\n", j.Kind, j.Key, err)
		}
	}
}

func queueOwnerDead(path string) bool {
	parts := strings.Split(filepath.Base(path), ".")
	if len(parts) < 5 {
		return false
	}
	pid, err := strconv.Atoi(parts[len(parts)-3])
	if err != nil || pid <= 0 {
		return false
	}
	start, err := proc.StartTime(pid)
	if err != nil {
		return errors.Is(err, proc.ErrNoProcess)
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(start))) != parts[len(parts)-2]
}

func failJob(path string) {
	dir := filepath.Join(queueDir(), "failed")
	if os.MkdirAll(dir, 0o700) == nil {
		_ = os.Rename(path, filepath.Join(dir, jobNameFromClaim(path)))
	}
}

func jobNameFromClaim(path string) string {
	name, _, _ := strings.CutLast(filepath.Base(path), ".json.")
	return name + ".json"
}
