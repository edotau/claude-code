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

// runningSegment is "⚙ N running (A agents · L legs)": A = this session's live subagent markers, L = live
// dispatch jobs machine-wide. Empty when nothing runs.
func runningSegment(in Input) string {
	stamp := stampFile("running", paths.StateDir()+"\x00"+in.TranscriptPath)
	var agents, legs int
	if f := freshStamp(stamp, runningTTL, 2); f != nil {
		agents, _ = strconv.Atoi(f[0])
		legs, _ = strconv.Atoi(f[1])
	} else {
		agents = inflight.CountOwned(inflight.SessionOwner(), inflight.SubagentDir(in.TranscriptPath), time.Now())
		legs = liveDispatches()
		writeTimedStamp(stamp, strconv.Itoa(agents), strconv.Itoa(legs))
	}
	if agents+legs == 0 {
		return ""
	}
	return fmt.Sprintf("⚙ %d running (%d agents · %d legs)", agents+legs, agents, legs)
}

// liveDispatches counts dispatch records whose job is alive; it mirrors agent.Record.Alive because agent
// imports hooks, which imports this package.
func liveDispatches() int {
	dir := filepath.Join(paths.StateDir(), "dispatch")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
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
		}
		if json.Unmarshal(b, &r) != nil || r.Status != "running" {
			continue
		}
		if (r.PID > 0 && proc.Alive(r.PID)) || (r.PID <= 0 && time.Since(r.Time) < time.Minute) {
			n++
		}
	}
	return n
}
