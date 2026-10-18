package worktree

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// MergeLockFile is the one harness lock serializing the fast-forward promote step across concurrent
// `worktree merge` runs. It coordinates this harness's own operations only, never an external writer.
const MergeLockFile = "worktree-merge.lock"

// MergeLockWait bounds how long Acquire waits for the lock before giving up with ErrMergeBusy.
const MergeLockWait = 5 * time.Second

// ErrMergeBusy is returned when the merge lock is not granted within the wait.
var ErrMergeBusy = errors.New("worktree merge: another promote holds the merge lock")

// MergeLockPath is the physical lock file under paths.StateDir().
func MergeLockPath() string { return filepath.Join(paths.StateDir(), MergeLockFile) }

// Acquire runs fn holding the one merge lock; wait<=0 means MergeLockWait. Not reentrant: fn must
// never call Acquire itself, or a single caller would deadlock on its own held lock.
func Acquire(wait time.Duration, fn func() error) error {
	dir := paths.StateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if wait <= 0 {
		wait = MergeLockWait
	}
	ran, err := paths.TryFileLock(MergeLockPath(), wait, fn)
	if err == nil && !ran {
		return ErrMergeBusy
	}
	return err
}
