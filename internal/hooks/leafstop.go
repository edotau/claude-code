// The leaf-stop gate: a leaf may not stop while a background dispatch its own transcript started is still alive.
package hooks

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// leafStopMaxWait bounds the wait from the dispatch's start, so a hung job cannot trap a leaf.
const leafStopMaxWait = 2 * time.Hour

// jobIDPattern matches the id `ask --background` prints, as text or as the --format json "id" field.
var jobIDPattern = regexp.MustCompile(`job (\d{8}T\d{6}\.\d{3}-[A-Za-z0-9._-]+) started|\\"id\\":\\"(\d{8}T\d{6}\.\d{3}-[A-Za-z0-9._-]+)\\"`)

// dispatchRecord is the slice of an agent.Record the gate reads (agent imports hooks, so it cannot be imported here).
type dispatchRecord struct {
	ID     string    `json:"id"`
	Time   time.Time `json:"time"`
	Status string    `json:"status"`
	PID    int       `json:"pid"`
}

// alive mirrors agent.Record.Alive: running with a live pid, or not yet recorded within a minute.
func (r dispatchRecord) alive() bool {
	switch {
	case r.Status != "running":
		return false
	case r.PID > 0:
		return proc.Alive(r.PID)
	}
	return time.Since(r.Time) < time.Minute
}

// loadDispatch reads state/dispatch/<id>.json by its exact id; tests replace it.
var loadDispatch = func(id string) (dispatchRecord, error) {
	var r dispatchRecord
	b, err := os.ReadFile(filepath.Join(paths.StateDir(), "dispatch", id+".json"))
	if err == nil {
		err = json.Unmarshal(b, &r)
	}
	return r, err
}

// leafJobIDs lists the job ids the leaf's Bash tool results printed, in order, from its transcript.
func leafJobIDs(transcript string) ([]string, error) {
	f, err := os.Open(transcript)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var ids []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 64<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, `"tool_result"`) {
			continue
		}
		for _, m := range jobIDPattern.FindAllStringSubmatch(line, -1) {
			if id := m[1] + m[2]; !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids, sc.Err()
}

// leafStopBlock returns a Block while a dispatch the stopping leaf started is alive; any error fails open.
func leafStopBlock(in *Input, agentID string) (Result, bool) {
	if in.Agent() != AgentClaude || in.AgentTranscriptPath == "" {
		return Result{}, false
	}
	ids, err := leafJobIDs(in.AgentTranscriptPath)
	if err != nil {
		return Result{}, false
	}
	for _, id := range ids {
		rec, err := loadDispatch(id)
		if err != nil || !rec.alive() {
			continue
		}
		return leafStopVerdict(agentID, rec)
	}
	return Result{}, false
}

// leafStopVerdict blocks until the wait limit, then allows the stop with a visible note.
func leafStopVerdict(agentID string, rec dispatchRecord) (Result, bool) {
	if time.Since(rec.Time) >= leafStopMaxWait {
		return Result{SystemMessage: fmt.Sprintf("leaf %s stopped with dispatch %s still running (%s limit reached)", agentID, rec.ID, leafStopMaxWait)}, false
	}
	return Result{Decision: Block, Reason: fmt.Sprintf("Dispatch %s you started is still running. Run `claude-code agents tail -f %s` "+
		"with a 600000ms Bash timeout, repeating until it finishes, then report its result; do not stop before it reaches a terminal status.", rec.ID, rec.ID)}, true
}
