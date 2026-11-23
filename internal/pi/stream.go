package pi

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
)

// piEvent is one line of `pi --mode json`; only the fields the parser and the driver read are decoded.
type piEvent struct {
	Type string `json:"type"`
	// session: the session id
	ID string `json:"id"`
	// agent_settled: the run was cancelled
	Aborted    bool            `json:"aborted"`
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
		Type  string          `json:"type"`
		Delta json.RawMessage `json:"delta"`
	} `json:"assistantMessageEvent"`
}

// piUsage is an assistant message_end usage; output already counts reasoning and cacheWrite counts cacheWrite1h.
type piUsage struct {
	Input, Output, CacheRead, CacheWrite, CacheWrite1h, Reasoning int
	Cost                                                          struct{ Total float64 }
}

// parseEvents turns Pi's JSONL into wire events; the exit code says nothing, so the stream alone decides failure.
func parseEvents(br *bufio.Reader, sink wire.Sink, onMeaningful func(), onEvent func(piEvent) error, logs ...wire.StreamLog) (res wire.Result, _ error) {
	t := &piTurn{open: map[string]string{}, sink: sink, lg: wire.PickLog(logs), res: &res, onMeaningful: onMeaningful, onEvent: onEvent}
	defer t.flushLog()
	tr, err := wire.DecodeEvents(br, t.lg, "decode pi json", true, nil, t.handle)
	res.Transport = tr
	if err != nil {
		res.Answer = t.text()
		return res, err
	}
	err = t.finish(false)
	res.Answer = t.text()
	return res, err
}

// piTurn is parseEvents' state across one run's events.
type piTurn struct {
	sink         wire.Sink
	lg           wire.StreamLog
	res          *wire.Result
	answer       []string // one entry per assistant message that spoke
	cur          strings.Builder
	logLine      strings.Builder
	stop         string // the last assistant message's stop reason; only the last one decides the run
	errMsg       string // and its error message
	assistant    bool
	open         map[string]string // running tool calls, id to write path
	progressed   bool
	finished     bool
	aborted      bool
	fired        bool
	onMeaningful func()
	onEvent      func(piEvent) error
}

func (t *piTurn) handle(ev piEvent, ts *wire.TransportTracker) (bool, error) {
	if t.onEvent != nil {
		if err := t.onEvent(ev); err != nil { // an error stops the stream before this event is forwarded
			return false, err
		}
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
		if ev.Update == nil {
			return false
		}
		d, err := deltaText(ev.Update.Delta)
		return err == nil && strings.TrimSpace(d) != ""
	case "message_end":
		return ev.Message != nil && ev.Message.Role == "assistant" && ev.Message.Usage.Output > 0
	}
	return false
}

func (t *piTurn) apply(ev piEvent, ts *wire.TransportTracker) error {
	switch ev.Type {
	case "agent_start", "turn_start", "message_start", "message_update", "message_end", "tool_execution_start", "auto_retry_start":
		t.finished = false // work after agent_settled voids that judgment; only a later agent_settled settles the run
	}
	switch ev.Type {
	case "session":
		t.res.SessionID = ev.ID
		return t.sink.Emit(wire.Event{Type: wire.EventStart, SessionID: ev.ID})
	case "message_update":
		return t.delta(ev)
	case "tool_execution_start":
		t.flushLog()
		t.lg.Emit("tool", ev.ToolName)
		t.progressed = true
		t.open[ev.ToolCallID] = ev.writePath()
		return t.sink.Emit(wire.ToolEvent(ev.ToolName, ev.ToolCallID))
	case "tool_execution_end":
		return t.toolEnd(ev)
	case "auto_retry_start":
		t.flushLog()
		ts.Retry(nil)
		text := fmt.Sprintf("attempt %d/%d retry_in=%dms", ev.Attempt, ev.MaxAttempts, ev.DelayMs)
		t.lg.Emit("retry", text)
		return t.sink.Emit(wire.Event{Type: wire.EventRetry, Text: text})
	case "message_end":
		return t.messageEnd(ev)
	case "agent_settled":
		// agent_end is not final: overflow compaction and retries continue after it; agent_settled ends the run
		t.aborted = ev.Aborted
		return t.finish(true)
	}
	return nil
}

func (t *piTurn) toolEnd(ev piEvent) error {
	t.flushLog()
	path := cmp.Or(t.open[ev.ToolCallID], ev.writePath())
	delete(t.open, ev.ToolCallID)
	status := "completed"
	if ev.IsError {
		status = "error"
	} else if path != "" {
		t.res.Written = append(t.res.Written, path)
	}
	t.lg.Emit("tool", ev.ToolName+": "+status)
	return t.sink.Emit(wire.Event{Type: wire.EventToolResult, Tool: &wire.ToolRef{Name: ev.ToolName, ID: ev.ToolCallID}, Status: status})
}

func (ev piEvent) writePath() string {
	if ev.ToolName != "write" && ev.ToolName != "edit" || len(ev.Args) == 0 {
		return ""
	}
	var a struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(ev.Args, &a)
	return strings.TrimPrefix(a.Path, "@")
}

// deltaText is a delta's text: a string, or the text parts of a content array (qwen sends one); else an error.
func deltaText(raw json.RawMessage) (string, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var parts []struct{ Text string }
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

// delta streams assistant text; a later message's first chunk starts a new turn.
func (t *piTurn) delta(ev piEvent) error {
	if ev.Update == nil || ev.Update.Type != "text_delta" || len(ev.Update.Delta) == 0 {
		return nil
	}
	d, err := deltaText(ev.Update.Delta)
	if err != nil {
		return fmt.Errorf("decode pi text_delta: %w", err)
	}
	if d == "" {
		return nil
	}
	sep := t.cur.Len() == 0 && len(t.answer) > 0
	t.cur.WriteString(d)
	t.progressed = true
	t.logDelta(d)
	return t.sink.Emit(wire.Event{Type: wire.EventText, Text: d, Sep: sep})
}

// logDelta keeps token boundaries out of the log without delaying foreground text events.
func (t *piTurn) logDelta(text string) {
	if t.lg == nil {
		return
	}
	for text != "" {
		line, rest, newline := strings.Cut(text, "\n")
		t.logLine.WriteString(line)
		if !newline {
			return
		}
		t.flushLog()
		text = rest
	}
}

func (t *piTurn) flushLog() {
	if t.logLine.Len() > 0 {
		t.lg.Emit("assistant", t.logLine.String())
		t.logLine.Reset()
	}
}

func (t *piTurn) messageEnd(ev piEvent) error {
	m := ev.Message
	if m == nil || m.Role != "assistant" {
		return nil
	}
	t.res.Turns++
	t.flushLog()
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
	t.assistant, t.stop, t.errMsg = true, m.StopReason, m.ErrorMessage
	return nil
}

// text is the answer so far, including a message that was cut off before its message_end.
func (t *piTurn) text() string {
	return strings.TrimSpace(strings.Join(append(slices.Clone(t.answer), t.cur.String()), "\n"))
}

// stopFailure accepts only pi-ai's stop reason "stop"; unknown reasons fail.
func stopFailure(reason, errMsg string) string {
	switch reason {
	case "stop":
		return ""
	case "error":
		return cmp.Or(errMsg, "pi request failed")
	case "aborted":
		return cmp.Or(errMsg, "pi request aborted")
	case "length":
		return "pi response truncated: the model reached its output token limit"
	case "toolUse":
		return "pi stopped with a tool call pending"
	}
	return fmt.Sprintf("pi ended on unfinished stop reason %q", reason)
}

// failure is why the run did not finish well, or "" when the last message stopped cleanly and nothing is left open.
func (t *piTurn) failure() string {
	switch {
	case !t.assistant:
		return "pi ended without an answer"
	case t.aborted:
		return "pi run aborted"
	case len(t.open) > 0:
		return "pi left a tool call running"
	}
	return stopFailure(t.stop, t.errMsg)
}

// finish settles the run once; without agent_settled (a cut-off stream) even a clean-looking run is an error.
func (t *piTurn) finish(settled bool) error {
	t.flushLog()
	if t.finished {
		return nil
	}
	t.finished = true
	msg := t.failure()
	if !settled && t.assistant {
		msg = "pi ended before finishing: " + cmp.Or(msg, "no final event")
	}
	if msg == "" {
		return nil
	}
	if err := wire.EmitError(t.lg, t.sink, msg); err != nil {
		return err
	}
	return retry.Classify("pi", errors.New(msg), t.progressed)
}
