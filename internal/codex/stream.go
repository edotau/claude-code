package codex

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
)

type fileChange struct {
	Path string `json:"path"`
}

type codexEvent struct {
	Type      string `json:"type"`
	ThreadID  string `json:"thread_id"`
	Message   string `json:"message"`
	WillRetry bool   `json:"will_retry"`
	Item      struct {
		ID               string       `json:"id"`
		Type             string       `json:"type"` // agent_message | command_execution | error | reasoning | ...
		Text             string       `json:"text"`
		Command          string       `json:"command"`
		Status           string       `json:"status"`
		AggregatedOutput string       `json:"aggregated_output"`
		ExitCode         *int         `json:"exit_code"`
		Message          string       `json:"message"`
		Changes          []fileChange `json:"changes"` // file_change: the files the item touched
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
	retryError   string
	log          wire.StreamLog
	sink         wire.Sink
	onMeaningful func()
	idle         *agent.IdleTimeout
	running      map[string]bool // tool items started and not yet completed
	marked       map[string]bool // non-shell tool items already marked as replay progress
	turnDone     bool            // the last turn ended with turn.completed
}

// parseCodexStream emits text, tools and usage, retaining the last message as the answer.
func parseCodexStream(br *bufio.Reader, sink wire.Sink, onMeaningful func(), logs ...wire.StreamLog) (wire.Result, error) {
	return parseCodexIdle(nil)(br, sink, onMeaningful, logs...)
}

// parseCodexIdle is parseCodexStream that feeds every event to idle: a nil idle is no watch.
func parseCodexIdle(idle *agent.IdleTimeout) func(*bufio.Reader, wire.Sink, func(), ...wire.StreamLog) (wire.Result, error) {
	return func(br *bufio.Reader, sink wire.Sink, onMeaningful func(), logs ...wire.StreamLog) (res wire.Result, _ error) {
		state := codexStreamState{log: wire.PickLog(logs), sink: sink, onMeaningful: onMeaningful, idle: idle, running: map[string]bool{}, marked: map[string]bool{}}
		return state.parse(br)
	}
}

func (s *codexStreamState) parse(br *bufio.Reader) (res wire.Result, _ error) {
	tr, err := wire.DecodeEvents(br, s.log, "decode codex json", true, nil, func(ev codexEvent, _ *wire.TransportTracker) (bool, error) {
		s.observe(ev)
		return false, s.apply(ev)
	})
	res = s.res
	res.Transport, res.Answer = tr, s.ans.Last()
	if err != nil {
		return res, err
	}
	s.failure = cmp.Or(s.failure, s.retryError)
	if s.failure != "" {
		return res, errors.New(s.failure)
	}
	if !s.turnDone { // interim text without turn.completed is a cut-off run, not an answer
		return res, fmt.Errorf("codex: stream ended without turn.completed: %w", io.ErrUnexpectedEOF)
	}
	return res, nil
}

// observe re-arms the idle window per event: a retry notice is no progress, a running tool holds the window off.
func (s *codexStreamState) observe(ev codexEvent) {
	if s.idle == nil || ev.WillRetry {
		return
	}
	if strings.HasPrefix(ev.Type, "item.") {
		s.idle.Spoke()
		if codexToolItem(ev.Item.Type) {
			switch ev.Type {
			case "item.started":
				s.running[ev.Item.ID] = true
			case "item.completed":
				delete(s.running, ev.Item.ID)
			}
		}
	}
	if len(s.running) > 0 { // a long tool call emits nothing until it ends
		s.idle.Pause()
		return
	}
	s.idle.Beat(ev.Type)
}

func codexToolItem(typ string) bool {
	return typ == "command_execution" || typ == "file_change" || typ == "mcp_tool_call" || typ == "collab_tool_call" || typ == "web_search"
}

func (s *codexStreamState) apply(ev codexEvent) error {
	if err := s.markTool(ev); err != nil {
		return err
	}
	switch ev.Type {
	case "thread.started":
		s.res.SessionID = ev.ThreadID
	case "item.completed":
		return s.completedItem(ev)
	case "item.started":
		return s.startedItem(ev)
	case "turn.started":
		s.turnDone = false
	case "turn.completed":
		s.turnDone = true
		s.retryError = ""
		u := ev.Usage
		s.res.Usage.InputTokens += max(0, u.InputTokens-u.CachedInputTokens)
		s.res.Usage.CacheReadTokens += u.CachedInputTokens
		s.res.Usage.OutputTokens += u.OutputTokens
		s.log.Emit("result", fmt.Sprintf("turn finished, tokens in=%d out=%d", u.InputTokens, u.OutputTokens))
	case "turn.failed":
		s.failure = cmp.Or(ev.Message, ev.Error.Message, "codex turn failed")
		return wire.EmitError(s.log, s.sink, s.failure)
	case "error":
		message := cmp.Or(ev.Message, ev.Error.Message, "codex turn failed")
		s.retryError = message
		if !ev.WillRetry {
			return wire.EmitError(s.log, s.sink, message)
		}
		s.log.Emit("warning", message)
		return s.sink.Emit(wire.Event{Type: wire.EventProgress, Text: message, Status: "warning"})
	}
	return nil
}

// markTool emits one bare progress mark per non-shell tool item, so a retry never replays its effects.
func (s *codexStreamState) markTool(ev codexEvent) error {
	it := ev.Item
	if ev.Type != "item.started" && ev.Type != "item.completed" || it.Type == "command_execution" ||
		!codexToolItem(it.Type) || s.marked[it.ID] {
		return nil
	}
	s.marked[it.ID] = true
	s.log.Emit("tool", it.Type)
	return s.sink.Emit(wire.Event{Type: wire.EventProgress, Text: it.Type})
}

func (s *codexStreamState) startedItem(ev codexEvent) error {
	if ev.Item.Type != "command_execution" {
		return nil
	}
	tool := wire.ToolEvent("shell", ev.Item.ID)
	tool.Text = ev.Item.Command
	if err := s.sink.Emit(tool); err != nil {
		return err
	}
	s.log.Emit("tool", ev.Item.Command)
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
	case "file_change":
		s.collectChanges(it.Status, it.Changes)
	case "error":
		message := cmp.Or(it.Message, it.Text, "codex item error")
		s.log.Emit("warning", message)
		return s.sink.Emit(wire.Event{Type: wire.EventProgress, Text: message, Status: "warning"})
	}
	return nil
}

func (s *codexStreamState) collectChanges(status string, changes []fileChange) {
	if status != "completed" {
		return
	}
	for _, c := range changes {
		s.res.Written = append(s.res.Written, c.Path)
	}
}
