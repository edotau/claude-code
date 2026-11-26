// Package health is the one record of dispatch health (R34): whether dispatch itself is down and which tasks failed twice.
package health

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// The closed list of reasons; a budget end is Budget(kind).
const (
	ProxyDown      = "proxy_down"
	ProxyUnhealthy = "proxy_unhealthy"
	RouterSecret   = "router_secret"
	EnsureFailed   = "ensure_failed"
	TwoFailed      = "two_failed_attempts"

	// MaxAttempts is the failed attempts of one task after which it is degraded.
	MaxAttempts = 2

	schemaVersion = 1
	taskWindow    = 6 * time.Hour // a task's count expires when no attempt failed within it
	taskKeep      = 200
)

// Budget is the reason for a task whose run ended on the budget kind (time, tokens, output, context).
func Budget(kind string) string { return "budget_" + kind }

// Notice is the one line that replaces a resend hint while reason holds (R34).
func Notice(reason string) string {
	return "dispatch degraded: " + reason + "; continue in the main session"
}

// Report publishes a newly set degraded state to the failure log; internal/worktree sets it (it imports this tree).
var Report func(reason, subject string)

type down struct {
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

type task struct {
	Key      string    `json:"key"`
	PlanTask string    `json:"plan_task,omitempty"`
	N        int       `json:"n"`
	Reason   string    `json:"reason,omitempty"`
	Since    time.Time `json:"since"`
	Time     time.Time `json:"time"`
}

type file struct {
	Schema int    `json:"schema"`
	Down   *down  `json:"down,omitempty"`
	Tasks  []task `json:"tasks,omitempty"`
}

// Status is what a caller asks: degraded or not, why, since when, and the task's failed attempts.
type Status struct {
	Degraded bool
	Reason   string
	Since    time.Time
	Attempts int
}

func path() string { return filepath.Join(paths.StateDir(), "agent", "health.json") }

var format = schemas.Register(schemas.Format{
	Name: "dispatch-health", Owner: "internal/agent", Kind: schemas.Cache, Version: schemaVersion,
	Glob: path,
	New:  func() any { return new(file) },
	Keep: "rebuildable cache; the degraded state clears on a passing health check, task counts expire after 6 hours",
})

// read returns the file; a missing file is empty, an unreadable one is empty and warned about.
func read() file {
	b, err := os.ReadFile(path())
	switch {
	case errors.Is(err, os.ErrNotExist):
		return file{}
	case err != nil:
		fmt.Fprintf(os.Stderr, "claude-code: dispatch health unreadable: %v\n", err)
		return file{}
	}
	var f file
	if !schemas.DecodeOK(format, b, &f) {
		fmt.Fprintln(os.Stderr, "claude-code: dispatch health unreadable: bad or newer schema")
		return file{}
	}
	return f
}

// update edits the file under its lock and writes it only when edit reports a change.
func update(now time.Time, edit func(*file) (changed bool)) {
	p := path()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "claude-code: dispatch health not updated: %v\n", err)
		return
	}
	err := paths.WithFileLock(p, func() error {
		f := read()
		f.Tasks = slices.DeleteFunc(f.Tasks, func(t task) bool { return now.Sub(t.Time) >= taskWindow })
		if !edit(&f) {
			return nil
		}
		f.Schema = schemaVersion
		b, err := json.Marshal(f)
		if err != nil {
			return err
		}
		return paths.AtomicWrite(p, b, 0o600)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code: dispatch health not updated: %v\n", err)
	}
}

func publish(reason, subject string) {
	if Report != nil {
		Report(reason, subject)
	}
}

// Degrade marks dispatch itself down; the first reason stands until a health check clears it.
func Degrade(reason string) {
	var fresh bool
	update(time.Now(), func(f *file) bool {
		if f.Down != nil {
			return false
		}
		f.Down, fresh = &down{Reason: reason, Since: time.Now()}, true
		return true
	})
	if fresh {
		publish(reason, "dispatch")
	}
}

// ClearDown ends a dispatch-down state a liveness probe disproves; router_secret needs a run through the proxy.
func ClearDown() { clearDown(true) }

// clearDown clears the outage it observed, never a newer one a peer published since.
func clearDown(liveness bool) {
	d := read().Down
	if d == nil || liveness && d.Reason == RouterSecret {
		return // the common call writes nothing
	}
	clearObserved(d)
}

func clearObserved(d *down) {
	update(time.Now(), func(f *file) bool {
		if f.Down == nil || !f.Down.Since.Equal(d.Since) {
			return false
		}
		f.Down = nil
		return true
	})
}

// Fail counts one failed attempt of the task key; a budget end (kind != "") or the second failure degrades the task.
func Fail(key, planTask, budgetKind string) {
	var reason string
	update(time.Now(), func(f *file) bool {
		i := slices.IndexFunc(f.Tasks, func(t task) bool { return t.Key == key })
		if i < 0 {
			f.Tasks = append(f.Tasks, task{Key: key, PlanTask: planTask, Since: time.Now()})
			i = len(f.Tasks) - 1
		}
		t := f.Tasks[i]
		f.Tasks = slices.Delete(f.Tasks, i, i+1)
		t.N, t.Time = t.N+1, time.Now()
		switch {
		case t.Reason != "":
		case budgetKind != "":
			t.Reason, reason = Budget(budgetKind), Budget(budgetKind)
		case t.N >= MaxAttempts:
			t.Reason, reason = TwoFailed, TwoFailed
		}
		f.Tasks = append(f.Tasks, t)[max(0, len(f.Tasks)+1-taskKeep):] // newest failure last; the oldest drop first
		return true
	})
	if reason != "" {
		publish(reason, subject(planTask, key))
	}
}

func subject(planTask, key string) string {
	if planTask != "" {
		return planTask
	}
	return key[:min(12, len(key))]
}

// Succeed clears the task's count; a run that went through the proxy also ends a dispatch-down state.
func Succeed(key string, viaProxy bool) {
	if slices.ContainsFunc(read().Tasks, func(t task) bool { return t.Key == key }) {
		update(time.Now(), func(f *file) bool {
			n := len(f.Tasks)
			f.Tasks = slices.DeleteFunc(f.Tasks, func(t task) bool { return t.Key == key })
			return len(f.Tasks) != n
		})
	}
	if viaProxy {
		clearDown(false)
	}
}

// Of is the health for a task key: its own degraded state first, else the global one. It never fails.
func Of(key string) Status {
	f, now := read(), time.Now()
	var st Status
	for _, t := range f.Tasks {
		if t.Key == key && now.Sub(t.Time) < taskWindow {
			st.Attempts = t.N
			if t.Reason != "" {
				return Status{Degraded: true, Reason: t.Reason, Since: t.Since, Attempts: t.N}
			}
		}
	}
	if f.Down != nil {
		st.Degraded, st.Reason, st.Since = true, f.Down.Reason, f.Down.Since
	}
	return st
}
