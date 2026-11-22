package serve

import (
	"errors"
	"os"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

const watchQueryTimeout = 20 * time.Millisecond

func init() { agent.WatchQueue = watchQueue }

func watchQueue(now time.Time) agent.QueueWatch {
	status, err := QueryStatus(watchQueryTimeout)
	return queueWatch(status, err, now)
}

func queueWatch(status Status, err error, now time.Time) agent.QueueWatch {
	out := agent.QueueWatch{Limit: requeueWindow}
	if err != nil {
		out.Gone = errors.Is(err, ErrNotRunning) || errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
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
