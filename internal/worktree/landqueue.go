package worktree

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// QueuedLane is a lane the lane-land hook could not land by itself: a to-do the main session works off.
type QueuedLane struct {
	Lane     string `json:"lane"`
	Repo     string `json:"repo"`
	Branch   string `json:"branch,omitempty"`
	Reason   string `json:"reason"`
	Detail   string `json:"detail,omitempty"`
	Task     string `json:"task,omitempty"`
	Next     string `json:"next"`
	QueuedAt int64  `json:"queued_at"`
}

type landQueue struct {
	Schema int          `json:"schema"`
	Lanes  []QueuedLane `json:"lanes"`
}

const landQueueSchema = 1

var _ = schemas.Register(schemas.Format{
	Name: "land-queue", Owner: "internal/worktree", Kind: schemas.Record, Version: landQueueSchema,
	Glob: LandQueueFile, New: func() any { return &landQueue{} },
	Keep: "one entry per lane; a land or the lane's removal drops it",
})

// LandQueueFile is the one queue file every session reads.
func LandQueueFile() string {
	dir := filepath.Join(paths.StateDir(), "worktree")
	return paths.MoveLegacy(filepath.Join(dir, "merge-queue.json"), filepath.Join(dir, "land-queue.json"))
}

// Enqueue adds q or replaces the entry for its lane, keeping the first queued-at.
func Enqueue(q QueuedLane) error {
	q.Lane = paths.Canon(q.Lane)
	if q.QueuedAt == 0 {
		q.QueuedAt = time.Now().Unix()
	}
	return editQueue(func(lanes []QueuedLane) []QueuedLane {
		if i := slices.IndexFunc(lanes, func(e QueuedLane) bool { return e.Lane == q.Lane }); i >= 0 {
			q.QueuedAt = lanes[i].QueuedAt
			return slices.Concat(lanes[:i], []QueuedLane{q}, lanes[i+1:])
		}
		return append(slices.Clone(lanes), q)
	})
}

// Dequeue drops lane's entry; a missing queue or entry is no error.
func Dequeue(lane string) error {
	lane = paths.Canon(lane)
	return editQueue(func(lanes []QueuedLane) []QueuedLane {
		return slices.DeleteFunc(slices.Clone(lanes), func(e QueuedLane) bool { return e.Lane == lane })
	})
}

// Queued lists main's queued lanes, dropping (and persisting the drop of) entries whose lane no longer exists.
func Queued(main string) []QueuedLane {
	q, err := readQueue(LandQueueFile())
	if err != nil || len(q.Lanes) == 0 {
		return nil
	}
	var out []QueuedLane
	gone := false
	for _, e := range q.Lanes {
		if !paths.IsDir(e.Lane) {
			gone = true
		} else if main == "" || paths.SamePath(e.Repo, main) {
			out = append(out, e)
		}
	}
	if gone { // best effort: a busy lock leaves the prune to the next read, so a prompt hook never waits on it
		_ = editQueueWithin(queuePruneWait, func(lanes []QueuedLane) []QueuedLane {
			return slices.DeleteFunc(slices.Clone(lanes), func(e QueuedLane) bool { return !paths.IsDir(e.Lane) })
		})
	}
	return out
}

// QueuedFor is lane's entry in main's queue, if any.
func QueuedFor(main, lane string) (QueuedLane, bool) {
	lanes := Queued(main)
	i := slices.IndexFunc(lanes, func(e QueuedLane) bool { return paths.SamePath(e.Lane, lane) })
	if i < 0 {
		return QueuedLane{}, false
	}
	return lanes[i], true
}

// queuePruneWait bounds how long a reader waits to drop removed lanes; queueEditWait bounds a writer.
const (
	queuePruneWait = 100 * time.Millisecond
	queueEditWait  = 10 * time.Second
)

func editQueue(fn func([]QueuedLane) []QueuedLane) error { return editQueueWithin(queueEditWait, fn) }

func editQueueWithin(wait time.Duration, fn func([]QueuedLane) []QueuedLane) error {
	file := LandQueueFile()
	return paths.WithFileLockWithin(file, wait, func() error {
		q, err := readQueue(file)
		if err != nil {
			return err
		}
		next := fn(q.Lanes)
		if len(next) == 0 {
			if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		return paths.WriteJSON(file, landQueue{Schema: landQueueSchema, Lanes: next})
	})
}

func readQueue(file string) (landQueue, error) {
	var q landQueue
	raw, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return q, nil
	}
	if err == nil {
		err = json.Unmarshal(raw, &q)
	}
	return q, err
}
