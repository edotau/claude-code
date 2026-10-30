package statusline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// runningTTL caps the marker + dispatch sweep at one per 3s across renders.
const runningTTL = 3 * time.Second

// runningSegment is "N ◉": N = the main thread + this session's live subagent markers + this
// session's live dispatch legs. Always shown while the "agents" segment is enabled.
func runningSegment(in Input) string {
	agents, legs := runningCounts(in)
	return fmt.Sprintf("%d ◉", 1+agents+legs)
}

// runningCounts is the live subagent markers and live dispatch legs, cached for runningTTL.
func runningCounts(in Input) (agents, legs int) {
	stamp := stampFile("running", paths.StateDir()+"\x00"+in.TranscriptPath)
	if f := freshStamp(stamp, runningTTL, 2); f != nil {
		agents, _ = strconv.Atoi(f[0])
		legs, _ = strconv.Atoi(f[1])
	} else {
		agents = inflight.CountOwned(inflight.SessionOwner(), time.Now())
		legs = liveDispatches()
		writeTimedStamp(stamp, strconv.Itoa(agents), strconv.Itoa(legs))
	}
	return agents, legs
}

// liveDispatches counts this session's dispatch records whose job is alive; it mirrors agent.Record.Alive
// because agent imports hooks, which imports this package.
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
		if json.Unmarshal(b, &r) != nil || r.Status != "running" || r.Owner != owner {
			continue
		}
		if (r.PID > 0 && proc.Alive(r.PID)) || (r.PID <= 0 && time.Since(r.Time) < time.Minute) {
			n++
		}
	}
	return n
}
