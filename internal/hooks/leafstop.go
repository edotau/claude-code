// The leaf-stop gate: a leaf may not stop while its background dispatch runs; a coworker needs a ran-on line from a tool result.
package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
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
	err = textutil.EachLineErr(f, func(b []byte) {
		line := string(b)
		if !strings.Contains(line, `"tool_result"`) {
			return
		}
		for _, m := range jobIDPattern.FindAllStringSubmatch(line, -1) {
			if id := m[1] + m[2]; !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	})
	return ids, err
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
	expired, block := liveDispatchVerdict(ids, agentID)
	if block {
		return expired, true
	}
	if res, block := coworkerRanOnVerdict(in, agentID); block || expired.SystemMessage == "" {
		return res, block
	}
	return expired, false
}

// liveDispatchVerdict blocks on the first live dispatch within its wait limit, else carries the first expiry note.
func liveDispatchVerdict(ids []string, agentID string) (expired Result, block bool) {
	for _, id := range ids {
		rec, err := loadDispatch(id)
		if err != nil || !inflight.DispatchAlive(rec.Status, rec.PID, rec.Time) {
			continue
		}
		res, block := leafStopVerdict(agentID, rec)
		if block {
			return res, true
		}
		if expired.SystemMessage == "" {
			expired = res
		}
	}
	return expired, false
}

// leafStopVerdict blocks until the wait limit, then allows the stop with a visible note.
func leafStopVerdict(agentID string, rec dispatchRecord) (Result, bool) {
	if time.Since(rec.Time) >= leafStopMaxWait {
		return Result{SystemMessage: fmt.Sprintf("leaf %s stopped with dispatch %s still running (%s limit reached)", agentID, rec.ID, leafStopMaxWait)}, false
	}
	return Result{Decision: Block, Reason: fmt.Sprintf("Dispatch %s you started is still running. Run `claude-code agents tail -f %s` "+
		"with a 600000ms Bash timeout, repeating until it finishes, then report its result; do not stop before it reaches a terminal status.", rec.ID, rec.ID)}, true
}

// ranOnPattern matches the `ask: ran on` stderr line or the --format json "ran_on" field, as a tool result printed it.
var ranOnPattern = regexp.MustCompile(`ask: ran on |\\"ran_on\\":`)

// leafBrief is the text of the leaf's first user message and whether a tool result printed a ran-on line.
func leafBrief(transcript string) (brief string, ranOn bool, err error) {
	f, err := os.Open(transcript)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	err = textutil.EachLineErr(f, func(b []byte) {
		line := string(b)
		switch {
		case brief == "" && strings.Contains(line, `"type":"user"`):
			var m struct {
				Message struct {
					Content any `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(b, &m) == nil {
				brief, _ = m.Message.Content.(string)
			}
		case strings.Contains(line, `"tool_result"`) && ranOnPattern.MatchString(line):
			ranOn = true
		}
	})
	return brief, ranOn, err
}

// leafAgentType is the stopping leaf's type: the payload's agent_type, else the meta.json beside its transcript.
func leafAgentType(in *Input) string {
	if t := in.nested("agent_type"); t != "" {
		return t
	}
	b, err := os.ReadFile(strings.TrimSuffix(in.AgentTranscriptPath, ".jsonl") + ".meta.json")
	if err != nil {
		return ""
	}
	var m struct {
		AgentType string `json:"agentType"`
	}
	_ = json.Unmarshal(b, &m)
	return m.AgentType
}

// requestsAgent is true when the brief asks for an external agent; `Agent: in-context` and a brief with no line do not.
func requestsAgent(brief string) bool {
	switch {
	case inContextBrief(brief):
		return false
	case strings.Contains(brief, "Routed by agent-effort"):
		return true
	}
	return agentLineRe.MatchString(brief) || briefProfile(brief) != ""
}

// coworkerRanOnVerdict blocks a coworker's stop once when no tool result carried a ran-on line; the repeat stop passes.
func coworkerRanOnVerdict(in *Input, agentID string) (Result, bool) {
	if leafAgentType(in) != "coworker" {
		return Result{}, false
	}
	brief, ranOn, err := leafBrief(in.AgentTranscriptPath)
	if err != nil || ranOn || !requestsAgent(brief) {
		return Result{}, false
	}
	if in.StopHookActive {
		return Result{SystemMessage: fmt.Sprintf("coworker %s: finished with no ran-on line", agentID)}, false
	}
	return Result{Decision: Block, Reason: "No tool result of yours carries an `ask: ran on` line (or a `ran_on` field), so the external " +
		"agent was never called. Run the `claude-code ask` the brief requests, or report BLOCKED with the reason; do not do the work in-context."}, true
}
