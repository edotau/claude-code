// `agent-routed` — SubagentStart: records the model a spawned subagent actually launched with,
// reconciling it against the oldest `agent-effort` decision for the session not yet claimed by an
// audit row. Best-effort and advisory: any I/O error is swallowed, the hook always proceeds.
package hooks

import (
	"cmp"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// routingDir holds the decision/audit trail agent-effort and agent-routed share.
func routingDir() string { return filepath.Join(paths.StateDir(), "routing") }

func decisionsPath() string { return filepath.Join(routingDir(), "decisions.jsonl") }
func auditPath() string     { return filepath.Join(routingDir(), "audit.jsonl") }

// decisionsCap: past this size decisions.jsonl rotates to decisions.jsonl.1 (one generation kept).
const decisionsCap = 1 << 20 // 1 MiB

// routingLockWait bounds the wait for the routing-dir lock; both hooks carry a 5s Timeout.
const routingLockWait = 2 * time.Second

// AgentRouted appends an audit.jsonl row for a subagent's actual launch model, reconciled against
// agent-effort's decision log. Always proceeds.
func AgentRouted(r io.Reader) int {
	in := ParseInput(r)
	agentID := cmp.Or(in.AgentID, in.nested("agentId"))
	agentType := cmp.Or(in.nested("agent_type"), in.nested("subagent_type"), in.nested("agentType"))
	model := cmp.Or(in.nested("model"), in.nested("tool_input", "model"))
	sessionID := in.SessionID

	if err := os.MkdirAll(routingDir(), 0o700); err != nil {
		return ExitProceed
	}
	_, _ = paths.TryFileLock(auditPath()+".lock", routingLockWait, func() error {
		rec := map[string]any{
			"agent_id": agentID, "agent_type": agentType, "model": model,
			"session_id": sessionID, "at": time.Now().Unix(),
		}
		var matches any
		if decision := pendingDecision(sessionID, model); decision != nil {
			rec["decision_id"], _ = decision["decision_id"].(string)
			if model != "" {
				decisionModel, _ := decision["model"].(string)
				matches = decisionModel == model
			}
		}
		rec["matches"] = matches
		line, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return appendJSONLine(auditPath(), line)
	})
	return ExitProceed
}

// appendDecision records one agent-effort pin so a later agent-routed can reconcile it; best-effort.
func appendDecision(rec map[string]any) {
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if err := os.MkdirAll(routingDir(), 0o700); err != nil {
		return
	}
	_, _ = paths.TryFileLock(decisionsPath()+".lock", routingLockWait, func() error {
		rotateIfOversize(decisionsPath())
		return appendJSONLine(decisionsPath(), line)
	})
}

// pendingDecision is the oldest decision for sessionID not yet referenced in audit.jsonl, preferring
// one whose recorded model equals actualModel when actualModel is known.
func pendingDecision(sessionID, actualModel string) map[string]any {
	used := map[string]bool{}
	for _, rec := range readJSONLRecords(auditPath()) {
		if id, _ := rec["decision_id"].(string); id != "" {
			used[id] = true
		}
	}
	var pending []map[string]any
	for _, rec := range readJSONLRecords(decisionsPath()) {
		sid, _ := rec["session_id"].(string)
		did, _ := rec["decision_id"].(string)
		if sid != sessionID || sid == "" || did == "" || used[did] {
			continue
		}
		pending = append(pending, rec)
	}
	if actualModel != "" {
		for _, rec := range pending {
			if m, _ := rec["model"].(string); m == actualModel {
				return rec
			}
		}
	}
	if len(pending) > 0 {
		return pending[0]
	}
	return nil
}

// readJSONLRecords tolerates a missing file and skips any malformed line.
func readJSONLRecords(path string) []map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []map[string]any
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil {
			out = append(out, rec)
		}
	}
	return out
}

// rotateIfOversize keeps one prior generation: decisions.jsonl.1 is overwritten, never appended to.
func rotateIfOversize(path string) {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() <= decisionsCap {
		return
	}
	_ = os.Rename(path, path+".1")
}

// appendJSONLine opens path O_APPEND (a single write() call is atomic for a line this short) and
// writes line+"\n"; callers already hold the routing-dir lock, so this never interleaves with a sibling.
func appendJSONLine(path string, line []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}
