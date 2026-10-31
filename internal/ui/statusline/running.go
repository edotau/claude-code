package statusline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc/inflight"
)

// runningTTL caps the marker + dispatch sweep at one per 3s across renders.
const runningTTL = 3 * time.Second

// runningSegment is "N ◉": N = the main thread + this session's live subagent markers + this
// session's live dispatch dispatches. Always shown while the "agents" segment is enabled.
func runningSegment(in Input) string {
	agents, dispatches := runningCounts(in)
	return fmt.Sprintf("%d ◉", 1+agents+dispatches)
}

// runningCounts is the live subagent markers and live dispatch dispatches, cached for runningTTL.
func runningCounts(in Input) (agents, dispatches int) {
	stamp := stampFile("running", paths.StateDir()+"\x00"+in.TranscriptPath)
	if f := freshStamp(stamp, runningTTL, 2); f != nil {
		agents, _ = strconv.Atoi(f[0])
		dispatches, _ = strconv.Atoi(f[1])
	} else {
		agents = inflight.CountOwned(inflight.SessionOwner(), time.Now())
		dispatches = liveDispatches()
		writeTimedStamp(stamp, strconv.Itoa(agents), strconv.Itoa(dispatches))
	}
	return agents, dispatches
}

// liveDispatches counts this session's dispatch records whose job is alive; agent's cone is too
// heavy for the status line, so the record is re-read here and shares inflight.DispatchAlive.
func liveDispatches() int {
	dir := filepath.Join(paths.StateDir(), "dispatch")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	owner := inflight.SessionOwner()
	n := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var r struct {
			Status string    `json:"status"`
			PID    int       `json:"pid"`
			Time   time.Time `json:"time"`
			Owner  int       `json:"owner"`
		}
		if json.Unmarshal(b, &r) != nil || r.Owner != owner {
			continue
		}
		if inflight.DispatchAlive(r.Status, r.PID, r.Time) {
			n++
		}
	}
	return n
}
