package serve

import (
	"errors"
	"os"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

const (
	watchQueryTimeout = 20 * time.Millisecond
	admitGrace        = 5 * time.Second // Admit's Direct fallback and mark clear land after requeueWindow
)

// Gone needs goneProbes absent answers goneGap apart: a restarting daemon answers again within Admit's re-dials.
var (
	goneProbes  = 3
	goneGap     = dialStep
	watchStatus = QueryStatus
)

func init() { agent.WatchQueue = watchQueue }

func watchQueue(now time.Time) agent.QueueWatch {
	status, err := watchStatus(watchQueryTimeout)
	for i := 1; i < goneProbes && absent(err); i++ {
		time.Sleep(goneGap)
		status, err = watchStatus(watchQueryTimeout)
	}
	return queueWatch(status, err, now)
}

// absent is positive evidence of no daemon; a timeout or other error is not.
func absent(err error) bool {
	return errors.Is(err, ErrNotRunning) || errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}

func queueWatch(status Status, err error, now time.Time) agent.QueueWatch {
	out := agent.QueueWatch{Limit: requeueWindow + admitGrace}
	if err != nil {
		out.Gone = absent(err)
		return out
	}
	out.Jobs = make(map[string]agent.QueuedJob)
	for _, pool := range status.Pools {
		for ahead, w := range pool.Waiting {
			out.Jobs[w.ID] = agent.QueuedJob{ID: w.ID, Pool: pool.Pool, Wait: now.Sub(w.Since), Ahead: ahead}
		}
	}
	return out
}
