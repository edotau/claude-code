package job

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/paths"
)

// ErrLiveBusy is the refusal of a second turn while one is in flight.
var ErrLiveBusy = errors.New("a turn is already in flight on this live session")

// errLiveCap is the refusal when every live slot is taken.
var errLiveCap = fmt.Errorf("%d live sessions already running (max %d): agents cancel one first", MaxLiveSessions, MaxLiveSessions)

// LiveSocket is the unix socket path of live session id.
func LiveSocket(id string) string { return filepath.Join(paths.DispatchDir(), id+wire.LiveSockSuffix) }

// liveCount counts live sessions still running.
func liveCount() (int, error) {
	recs, err := ReadRecent(1 << 20)
	n := 0
	for _, r := range recs {
		if r.Live && r.Alive() {
			n++
		}
	}
	return n, err
}

// CheckLiveCap errors when starting another session would pass MaxLiveSessions.
func CheckLiveCap() error {
	n, err := liveCount()
	if err != nil {
		return err
	}
	if n >= MaxLiveSessions {
		return errLiveCap
	}
	return nil
}

// ensureDispatchDir creates the 0700 dispatch dir.
func ensureDispatchDir() error { return os.MkdirAll(paths.DispatchDir(), 0o700) }

// AcquireLiveSlot takes one of MaxLiveSessions flock'd slot files (freed when the holder dies); the func releases it.
func AcquireLiveSlot() (func(), error) {
	if err := ensureDispatchDir(); err != nil {
		return nil, err
	}
	for i := range MaxLiveSessions {
		f, err := os.OpenFile(filepath.Join(paths.DispatchDir(), fmt.Sprintf("live-slot-%d", i)), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		if paths.TryLock(f) {
			return func() { _ = f.Close() }, nil
		}
		_ = f.Close()
	}
	return nil, errLiveCap
}
