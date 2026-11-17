package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
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

func queueDir() string { return filepath.Join(StateDir(), "queue") }

// jobName maps a job to its file name; key characters outside [A-Za-z0-9._-] become '_'.
func jobName(kind, key string) string {
	clean := strings.Map(func(r rune) rune {
		if r == '.' || r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return '_'
	}, key)
	return kind + "-" + clean + ".json"
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
	tmp := filepath.Join(dir, "."+filepath.Base(final)+"."+strconv.Itoa(os.Getpid()))
	if err := paths.AtomicWrite(tmp, b, 0o600); err != nil {
		return false, err
	}
	defer func() { _ = os.Remove(tmp) }()
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
	for _, c := range due {
		dst := filepath.Join(runDir, filepath.Base(c.path)+"."+strconv.Itoa(os.Getpid()))
		if os.Rename(c.path, dst) == nil {
			return c.j, dst, true
		}
	}
	return Job{}, "", false
}

// finish drops a completed running job.
func finish(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// requeueDead returns running jobs whose worker died to the queue with attempts+1; the third attempt goes to failed/.
func requeueDead() {
	runs, _ := filepath.Glob(filepath.Join(queueDir(), "running", "*.json.*"))
	for _, p := range runs {
		pid, err := strconv.Atoi(filepath.Ext(p)[1:])
		if err != nil || !ownerDead(pid) {
			continue
		}
		j, ok := readJob(p)
		j.Attempts++
		if !ok || j.Attempts >= queueMaxAttempts {
			failJob(p)
			continue
		}
		if _, err := queueAdd(queueDir(), j); err == nil {
			_ = os.Remove(p)
		}
	}
}

func failJob(path string) {
	dir := filepath.Join(queueDir(), "failed")
	if os.MkdirAll(dir, 0o700) == nil {
		_ = os.Rename(path, filepath.Join(dir, strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))))
	}
}
