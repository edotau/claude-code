package codex

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/edotau/claude-code/internal/agent"
)

// codexEvent mirrors the JSON events from codex exec.
type codexEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Item     struct {
		ID      string `json:"id"`
		Type    string `json:"type"` // agent_message | command_execution | error | reasoning | ...
		Text    string `json:"text"`
		Command string `json:"command"`
		Status  string `json:"status"`
	} `json:"item"`
	Usage struct {
		InputTokens       int `json:"input_tokens"`
		CachedInputTokens int `json:"cached_input_tokens"`
		OutputTokens      int `json:"output_tokens"`
	} `json:"usage"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

type codexStreamState struct {
	res          agent.Result
	ans          agent.Answer
	failure      string
	log          agent.StreamLog
	sink         agent.Sink
	onMeaningful func()
}

// parseCodexStream emits text, tools and usage, retaining the last message as the answer.
func parseCodexStream(br *bufio.Reader, sink agent.Sink, onMeaningful func(), logs ...agent.StreamLog) (res agent.Result, _ error) {
	ts := agent.TransportTracker{Start: agent.Clock()}
	defer func() { res.Transport = ts.Result() }()
	state := codexStreamState{log: agent.PickLog(logs), sink: sink, onMeaningful: onMeaningful}
	err := agent.DecodeLines(br, state.log, func(raw []byte) (bool, error) {
		var ev codexEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			return false, fmt.Errorf("decode codex json: %w", err)
		}
		ts.Event(agent.Clock())
		return false, state.apply(ev)
	})
	res = state.res
	res.Answer = state.ans.Last()
	if err != nil {
		return res, err
	}
	if state.failure != "" {
		return res, errors.New(state.failure)
	}
	return res, nil
}

func (s *codexStreamState) apply(ev codexEvent) error {
	switch ev.Type {
	case "thread.started":
		s.res.SessionID = ev.ThreadID
	case "item.completed":
		return s.completedItem(ev)
	case "item.started":
		if ev.Item.Type == "command_execution" {
			tool := agent.ToolEvent("shell", ev.Item.ID)
			tool.Text = ev.Item.Command
			if err := s.sink.Emit(tool); err != nil {
				return err
			}
			s.log.Emit("tool", ev.Item.Command)
		}
	case "turn.completed":
		u := ev.Usage
		s.res.Usage.InputTokens += u.InputTokens - u.CachedInputTokens
		s.res.Usage.CacheReadTokens += u.CachedInputTokens
		s.res.Usage.OutputTokens += u.OutputTokens
		s.log.Emit("result", fmt.Sprintf("turn finished, tokens in=%d out=%d", u.InputTokens, u.OutputTokens))
	case "turn.failed":
		s.failure = cmp.Or(ev.Error.Message, "codex turn failed")
		return agent.EmitError(s.log, s.sink, s.failure)
	}
	return nil
}

func (s *codexStreamState) completedItem(ev codexEvent) error {
	it := ev.Item
	switch it.Type {
	case "agent_message":
		if it.Text == "" {
			return nil
		}
		if s.onMeaningful != nil {
			s.onMeaningful()
		}
		if err := s.ans.Emit(s.sink, it.Text, true); err != nil {
			return err
		}
		s.log.Emit("assistant", it.Text)
	case "command_execution":
		return s.sink.Emit(agent.Event{Type: agent.EventToolResult, Tool: &agent.ToolRef{Name: "shell", ID: it.ID}, Status: it.Status})
	}
	return nil
}
