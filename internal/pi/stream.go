package pi

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
)

// piEvent is one line of `pi --mode json`; only the fields the parser and the driver read are decoded.
type piEvent struct {
	Type string `json:"type"`
	// session: the session id
	ID string `json:"id"`
	// agent_end: another attempt follows, so a held failure is not final yet
	WillRetry  bool            `json:"willRetry"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args"`
	IsError    bool            `json:"isError"`
	// auto_retry_start
	Attempt      int    `json:"attempt"`
	MaxAttempts  int    `json:"maxAttempts"`
	DelayMs      int    `json:"delayMs"`
	ErrorMessage string `json:"errorMessage"`
	Message      *struct {
		Role         string          `json:"role"`
		Model        string          `json:"model"`
		StopReason   string          `json:"stopReason"`
		ErrorMessage string          `json:"errorMessage"`
		Usage        piUsage         `json:"usage"`
		Content      json.RawMessage `json:"content"`
	} `json:"message"`
	Update *struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	} `json:"assistantMessageEvent"`
}

// piUsage is an assistant message_end usage; output already counts reasoning and cacheWrite counts cacheWrite1h.
type piUsage struct {
	Input, Output, CacheRead, CacheWrite, CacheWrite1h, Reasoning int
	Cost                                                          struct{ Total float64 }
}

// parseEvents turns Pi's JSONL into wire events; the exit code says nothing, so the stream alone decides failure.
func parseEvents(br *bufio.Reader, sink wire.Sink, onMeaningful func(), onEvent func(piEvent), logs ...wire.StreamLog) (res wire.Result, _ error) {
	t := &piTurn{sink: sink, lg: wire.PickLog(logs), res: &res, onMeaningful: onMeaningful, onEvent: onEvent}
	tr, err := wire.DecodeEvents(br, t.lg, "decode pi json", true, nil, t.handle)
	res.Transport = tr
	if err != nil {
		res.Answer = strings.TrimSpace(strings.Join(t.answer, "\n"))
		return res, err
	}
	err = t.finish()
	res.Answer = strings.TrimSpace(strings.Join(t.answer, "\n"))
	return res, err
}

// piTurn is parseEvents' state across one run's events.
type piTurn struct {
	sink         wire.Sink
	lg           wire.StreamLog
	res          *wire.Result
	answer       []string // one entry per assistant message that spoke
	cur          strings.Builder
	pending      string // the last failed assistant message's error, until a later success clears it
	answered     bool
	progressed   bool
	finished     bool
	fired        bool
	onMeaningful func()
	onEvent      func(piEvent)
}

func (t *piTurn) handle(ev piEvent, ts *wire.TransportTracker) (bool, error) {
	if t.onEvent != nil {
		t.onEvent(ev)
	}
	if !t.fired && t.onMeaningful != nil && isMeaningful(ev) {
		t.fired = true
		t.onMeaningful()
	}
	return false, t.apply(ev, ts)
}

// isMeaningful reports real model output; lifecycle and empty deltas only renew the idle timeout.
func isMeaningful(ev piEvent) bool {
	switch ev.Type {
	case "tool_execution_start":
		return true
	case "message_update":
		return ev.Update != nil && strings.TrimSpace(ev.Update.Delta) != ""
	case "message_end":
		return ev.Message != nil && ev.Message.Role == "assistant" && ev.Message.Usage.Output > 0
	}
	return false
}

func (t *piTurn) apply(ev piEvent, ts *wire.TransportTracker) error {
	switch ev.Type {
	case "session":
		t.res.SessionID = ev.ID
		return t.sink.Emit(wire.Event{Type: wire.EventStart, SessionID: ev.ID})
	case "message_update":
		return t.delta(ev)
	case "tool_execution_start":
		t.progressed = true
		return t.sink.Emit(wire.ToolEvent(ev.ToolName, ev.ToolCallID))
	case "tool_execution_end":
		status := "completed"
		if ev.IsError {
			status = "error"
		}
		return t.sink.Emit(wire.Event{Type: wire.EventToolResult, Tool: &wire.ToolRef{Name: ev.ToolName, ID: ev.ToolCallID}, Status: status})
	case "auto_retry_start":
		ts.Retry(nil)
		text := fmt.Sprintf("attempt %d/%d retry_in=%dms", ev.Attempt, ev.MaxAttempts, ev.DelayMs)
		t.lg.Emit("retry", text)
		return t.sink.Emit(wire.Event{Type: wire.EventRetry, Text: text})
	case "message_end":
		return t.messageEnd(ev)
	case "agent_end":
		if !ev.WillRetry {
			return t.finish()
		}
	}
	return nil
}

// delta streams assistant text; a later message's first chunk starts a new turn.
func (t *piTurn) delta(ev piEvent) error {
	if ev.Update == nil || ev.Update.Type != "text_delta" || ev.Update.Delta == "" {
		return nil
	}
	sep := t.cur.Len() == 0 && len(t.answer) > 0
	t.cur.WriteString(ev.Update.Delta)
	t.progressed = true
	t.lg.Emit("assistant", ev.Update.Delta)
	return t.sink.Emit(wire.Event{Type: wire.EventText, Text: ev.Update.Delta, Sep: sep})
}

func (t *piTurn) messageEnd(ev piEvent) error {
	m := ev.Message
	if m == nil || m.Role != "assistant" {
		return nil
	}
	if text := strings.TrimSpace(t.cur.String()); text != "" {
		t.answer = append(t.answer, text)
	}
	t.cur.Reset()
	t.res.Reported = cmp.Or(m.Model, t.res.Reported)
	u, tu := &t.res.Usage, m.Usage
	u.InputTokens += tu.Input
	u.CacheReadTokens += tu.CacheRead
	u.CacheCreationTokens += tu.CacheWrite
	u.OutputTokens += tu.Output
	u.CostUSD += tu.Cost.Total
	t.lg.Emit("result", fmt.Sprintf("message finished (%s), tokens in=%d out=%d cache_write_1h=%d",
		cmp.Or(m.StopReason, "unknown"), tu.Input, tu.Output, tu.CacheWrite1h))
	if m.StopReason == "error" {
		t.pending = cmp.Or(m.ErrorMessage, "pi request failed")
		return nil
	}
	t.pending = ""
	t.answered = true
	return nil
}

// finish settles the run once: a held failure becomes a classified error, no assistant message at all is one too.
func (t *piTurn) finish() error {
	if t.finished {
		return nil
	}
	t.finished = true
	msg := t.pending
	if msg == "" && !t.answered {
		msg = "pi ended without an answer"
	}
	if msg == "" {
		return nil
	}
	if err := wire.EmitError(t.lg, t.sink, msg); err != nil {
		return err
	}
	return retry.Classify("pi", errors.New(msg), t.progressed)
}
