package statusline

import (
	"strconv"
	"time"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
)

// runningTTL caps the marker + dispatch sweep at one per 3s across renders.
const runningTTL = 3 * time.Second

// runningDispatches is the live dispatch count for this session, cached for runningTTL.
func runningDispatches(in Input) int {
	stamp := stampFile("running", paths.StateDir()+"\x00"+in.TranscriptPath)
	if f := freshStamp(stamp, runningTTL, 1); f != nil {
		n, _ := strconv.Atoi(f[0])
		return n
	}
	n := inflight.LiveDispatches(inflight.SessionOwner())
	writeTimedStamp(stamp, strconv.Itoa(n))
	return n
}
