package hooks

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

type codexRouteRecord struct {
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

// CodexRoutingStart records that Codex trusted and executed the session's hook configuration.
func CodexRoutingStart(r io.Reader, _, stderr io.Writer) int {
	in := ParseInput(r)
	recordCodexRoute(in, codexRouteRecord{Event: "session-start", Model: in.nested("model")}, stderr)
	return ExitProceed
}

// CodexRoutingResult links a spawn call to a child only when its result supplies the child's ID.
func CodexRoutingResult(r io.Reader, _, stderr io.Writer) int {
	in := ParseInput(r)
	if codexSpawnTool(in.ToolName) {
		recordCodexRoute(in, codexRouteRecord{Event: "tool-result", ToolUseID: in.nested("tool_use_id"),
			AgentID: codexResultAgentID(in.doc["tool_response"])}, stderr)
	}
	return ExitProceed
}

// CodexSubagentStart records the reported model; absence of a causal join remains unknown.
func CodexSubagentStart(r io.Reader, _, stderr io.Writer) int {
	in := ParseInput(r)
	recordCodexRoute(in, codexRouteRecord{Event: "child-start", AgentID: in.AgentID, Model: in.nested("model")}, stderr)
	return ExitProceed
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
	if in.Vendor() != VendorCodex || in.SessionID == "" {
		return
	}
	record.SessionID, record.At = in.SessionID, time.Now().UnixMilli()
	path := filepath.Join(paths.StateDir(), "codex", fmt.Sprintf("routing-%x.jsonl", sha256.Sum256([]byte(in.SessionID))))
	if err := paths.WithFileLock(path, func() error { return appendCodexRoute(path, record) }); err != nil {
		fmt.Fprintln(stderr, "codex routing: audit unavailable")
	}
}

func appendCodexRoute(path string, record codexRouteRecord) error {
	records, err := readCodexRoutes(path)
	if err != nil {
		return err
	}
	records = append(records, record)
	write := []codexRouteRecord{record}
	write = append(write, codexRouteMatches(records)...)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, row := range write {
		if err := enc.Encode(row); err != nil {
			return errors.Join(err, f.Close())
		}
	}
	return f.Close()
}

func readCodexRoutes(path string) ([]codexRouteRecord, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var records []codexRouteRecord
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		var row codexRouteRecord
		if err := json.Unmarshal(scan.Bytes(), &row); err != nil {
			return nil, err
		}
		records = append(records, row)
	}
	return records, scan.Err()
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
