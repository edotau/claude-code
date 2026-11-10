package statusline

import (
	"strconv"
	"time"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
)

// runningTTL caps the marker + dispatch sweep at one per 3s across renders.
const runningTTL = 3 * time.Second

// runningCounts is the live subagent markers and live dispatch dispatches, cached for runningTTL.
func runningCounts(in Input) (agents, dispatches int) {
	stamp := stampFile("running", paths.StateDir()+"\x00"+in.TranscriptPath)
	if f := freshStamp(stamp, runningTTL, 2); f != nil {
		agents, _ = strconv.Atoi(f[0])
		dispatches, _ = strconv.Atoi(f[1])
	} else {
		agents = inflight.CountOwned(inflight.SessionOwner(), time.Now())
		dispatches = inflight.LiveDispatches(inflight.SessionOwner())
		writeTimedStamp(stamp, strconv.Itoa(agents), strconv.Itoa(dispatches))
	}
	return agents, dispatches
}
