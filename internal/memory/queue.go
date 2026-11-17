package memory

import (
	"crypto/rand"
	"crypto/sha256"
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

func queueDir() string { return filepath.Join(StateDir(), "queue") }

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

// finish drops a completed running job.
func finish(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// requeueDead returns running jobs whose worker died to the queue with attempts+1; the third attempt goes to failed/.
func requeueDead() {
	_, _ = paths.TryFileLock(filepath.Join(queueDir(), "recovery.lock"), 0, func() error {
		runs, _ := filepath.Glob(filepath.Join(queueDir(), "running", "*.json.*"))
		for _, p := range runs {
			if !queueOwnerDead(p) {
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
