package codex

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"

	"github.com/edotau/claude-code/internal/agent/wire"
)

// codexEvent mirrors the JSON events from codex exec.
type codexEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
	Item     struct {
		ID               string `json:"id"`
		Type             string `json:"type"` // agent_message | command_execution | error | reasoning | ...
		Text             string `json:"text"`
		Command          string `json:"command"`
		Status           string `json:"status"`
		AggregatedOutput string `json:"aggregated_output"`
		ExitCode         *int   `json:"exit_code"`
		Message          string `json:"message"`
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
	res          wire.Result
	ans          wire.Answer
	failure      string
	log          wire.StreamLog
	sink         wire.Sink
	onMeaningful func()
}

// parseCodexStream emits text, tools and usage, retaining the last message as the answer.
func parseCodexStream(br *bufio.Reader, sink wire.Sink, onMeaningful func(), logs ...wire.StreamLog) (res wire.Result, _ error) {
	state := codexStreamState{log: wire.PickLog(logs), sink: sink, onMeaningful: onMeaningful}
	tr, err := wire.DecodeEvents(br, state.log, "decode codex json", true, nil, func(ev codexEvent, _ *wire.TransportTracker) (bool, error) {
		return false, state.apply(ev)
	})
	res = state.res
	res.Transport, res.Answer = tr, state.ans.Last()
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
			tool := wire.ToolEvent("shell", ev.Item.ID)
			tool.Text = ev.Item.Command
			if err := s.sink.Emit(tool); err != nil {
				return err
			}
			s.log.Emit("tool", ev.Item.Command)
		}
	case "turn.completed":
		u := ev.Usage
		s.res.Usage.InputTokens += max(0, u.InputTokens-u.CachedInputTokens)
		s.res.Usage.CacheReadTokens += u.CachedInputTokens
		s.res.Usage.OutputTokens += u.OutputTokens
		s.log.Emit("result", fmt.Sprintf("turn finished, tokens in=%d out=%d", u.InputTokens, u.OutputTokens))
	case "error", "turn.failed":
		s.failure = cmp.Or(ev.Message, ev.Error.Message, "codex turn failed")
		return wire.EmitError(s.log, s.sink, s.failure)
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
		status := it.Status
		if it.ExitCode != nil {
			status = fmt.Sprintf("%s (exit %d)", status, *it.ExitCode)
		}
		return s.sink.Emit(wire.Event{Type: wire.EventToolResult,
			Tool: &wire.ToolRef{Name: "shell", ID: it.ID}, Text: it.AggregatedOutput, Status: status})
	case "error":
		message := cmp.Or(it.Message, it.Text, "codex item error")
		s.log.Emit("warning", message)
		return s.sink.Emit(wire.Event{Type: wire.EventProgress, Text: message, Status: "warning"})
	}
	return nil
}
