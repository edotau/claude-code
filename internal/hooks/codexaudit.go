package hooks

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

type codexRouteRecord struct {
	Schema         int    `json:"schema"`
	Event          string `json:"event"`
	SessionID      string `json:"session_id"`
	ToolUseID      string `json:"tool_use_id,omitempty"`
	AgentID        string `json:"agent_id,omitempty"`
	Model          string `json:"model,omitempty"`
	RequestedModel string `json:"requested_model,omitempty"`
	Effort         string `json:"effort,omitempty"`
	Signal         string `json:"signal,omitempty"`
	Matches        *bool  `json:"matches_router_decision"`
	At             int64  `json:"at"`
}

// nativeCodexRoutingStart records that Codex trusted and executed the session's hook configuration.
func nativeCodexRoutingStart(in *Input) Result {
	return codexRecorded(in, codexRouteRecord{Event: "session-start", Model: in.nested("model")})
}

// nativeCodexRoutingResult links a spawn call to a child only when its result supplies the child's ID.
func nativeCodexRoutingResult(in *Input) Result {
	if !codexSpawnTool(in.ToolName) {
		return Result{}
	}
	return codexRecorded(in, codexRouteRecord{Event: "tool-result", ToolUseID: in.nested("tool_use_id"),
		AgentID: codexResultAgentID(in.doc["tool_response"])})
}

// nativeCodexSubagentStart records the reported model; absence of a causal join remains unknown.
func nativeCodexSubagentStart(in *Input) Result {
	return codexRecorded(in, codexRouteRecord{Event: "child-start", AgentID: in.AgentID, Model: in.nested("model")})
}

// codexRecorded runs the audit and keeps its warning as the result's stderr.
func codexRecorded(in *Input, record codexRouteRecord) Result {
	var warn strings.Builder
	recordCodexRoute(in, record, &warn)
	return Result{Stderr: strings.TrimSpace(warn.String())}
}

func codexResultAgentID(raw any) string {
	if text, ok := raw.(string); ok {
		if json.Unmarshal([]byte(text), &raw) != nil {
			return ""
		}
	}
	result, _ := raw.(map[string]any)
	id, _ := result["agent_id"].(string)
	return id
}

func recordCodexRoute(in *Input, record codexRouteRecord, stderr io.Writer) {
	if in.Agent() != AgentCodex || in.SessionID == "" {
		return
	}
	record.SessionID, record.At = in.SessionID, time.Now().UnixMilli()
	path := filepath.Join(paths.StateDir(), "codex", fmt.Sprintf("routing-%x.jsonl", sha256.Sum256([]byte(in.SessionID))))
	if err := paths.WithFileLock(path, func() error { return appendCodexRoute(path, record) }); err != nil {
		fmt.Fprintln(stderr, "codex routing: audit unavailable")
	}
}

func appendCodexRoute(path string, record codexRouteRecord) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := dropTornRoute(path, data); err != nil {
		return err
	}
	records := append(parseCodexRoutes(data), record)
	write := []codexRouteRecord{record}
	write = append(write, codexRouteMatches(records)...)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, row := range write {
		row.Schema = codexRouteSchema
		if err := enc.Encode(row); err != nil {
			return errors.Join(err, f.Close())
		}
	}
	return f.Close()
}

// dropTornRoute truncates an unterminated trailing record in data (path's content) so the next append starts fresh.
func dropTornRoute(path string, data []byte) error {
	if len(data) == 0 || data[len(data)-1] == '\n' {
		return nil
	}
	return os.Truncate(path, int64(bytes.LastIndexByte(data, '\n')+1))
}

func parseCodexRoutes(data []byte) []codexRouteRecord {
	var records []codexRouteRecord
	for line := range bytes.SplitSeq(data[:bytes.LastIndexByte(data, '\n')+1], []byte{'\n'}) {
		var row codexRouteRecord
		if len(bytes.TrimSpace(line)) > 0 && schemas.DecodeOK(codexRouteFormat, line, &row) {
			records = append(records, row)
		}
	}
	return records
}

// Join by tool and child IDs, never by model or event arrival order.
func codexRouteMatches(records []codexRouteRecord) []codexRouteRecord {
	decisions, results, starts := map[string]codexRouteRecord{}, map[string]string{}, map[string]codexRouteRecord{}
	verified := map[string]bool{}
	for _, row := range records {
		switch row.Event {
		case "decision":
			decisions[row.ToolUseID] = row
		case "tool-result":
			results[row.ToolUseID] = row.AgentID
		case "child-start":
			starts[row.AgentID] = row
		case "verified":
			verified[row.ToolUseID] = true
		}
	}
	var matches []codexRouteRecord
	for toolID, decision := range decisions {
		agentID := results[toolID]
		start, exists := starts[agentID]
		if toolID == "" || agentID == "" || !exists || start.Model == "" || verified[toolID] {
			continue
		}
		match := decision.RequestedModel == start.Model
		matches = append(matches, codexRouteRecord{Event: "verified", SessionID: decision.SessionID,
			ToolUseID: toolID, AgentID: agentID, Model: start.Model, RequestedModel: decision.RequestedModel,
			Matches: &match, At: time.Now().UnixMilli()})
	}
	return matches
}

// codexRouteSchema is the codex routing audit's current schema version.
const codexRouteSchema = 1

var codexRouteFormat = schemas.Register(schemas.Format{
	Name: "codex-routing", Owner: "internal/hooks", Kind: schemas.Log,
	Glob:    func() string { return filepath.Join(paths.StateDir(), "codex", "routing-*.jsonl") },
	New:     func() any { return &codexRouteRecord{} },
	Version: codexRouteSchema, Clean: "codex-audit", Migrate: schemas.SameShape,
})
