package pi

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

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
	Result     json.RawMessage `json:"result"`
	IsError    bool            `json:"isError"`
	// message_update: the in-flight message's cumulative usage; tool_execution_end: how long the tool ran
	Usage      json.RawMessage `json:"usage"` // decoded leniently: a malformed one never aborts the stream
	DurationMs json.RawMessage `json:"durationMs"`
	// auto_retry_start
	Attempt      int    `json:"attempt"`
	MaxAttempts  int    `json:"maxAttempts"`
	DelayMs      int    `json:"delayMs"`
	ErrorMessage string `json:"errorMessage"`
	// compaction_start: why; auto_retry_end: whether the retries won and the last error
	Reason     string `json:"reason"`
	Success    *bool  `json:"success"`
	FinalError string `json:"finalError"`
	Message    *struct {
		Role         string          `json:"role"`
		Model        string          `json:"model"`
		StopReason   string          `json:"stopReason"`
		ErrorMessage string          `json:"errorMessage"`
		Usage        piUsage         `json:"usage"`
		Content      json.RawMessage `json:"content"`
	} `json:"message"`
	Update *struct {
		Type    string          `json:"type"`
		Delta   json.RawMessage `json:"delta"`
		Content json.RawMessage `json:"content"` // thinking_end: the whole block
	} `json:"assistantMessageEvent"`
}

// piUsage is an assistant message_end usage; output already counts reasoning and cacheWrite counts cacheWrite1h.
type piUsage struct {
	Input, Output, CacheRead, CacheWrite, CacheWrite1h, Reasoning int
	Cost                                                          struct{ Total float64 }
}

// durationMs is the tool's run time, 0 when absent or malformed.
func (ev piEvent) durationMs() int64 {
	var d int64
	_ = json.Unmarshal(ev.DurationMs, &d)
	return max(d, 0)
}

// compactedLine is "compacted 150000→32000" from compaction_end's result, bare "compacted" without both numbers.
func (ev piEvent) compactedLine() string {
	var r struct{ TokensBefore, EstimatedTokensAfter int }
	if json.Unmarshal(ev.Result, &r) != nil || r.TokensBefore <= 0 || r.EstimatedTokensAfter <= 0 {
		return "compacted"
	}
	return fmt.Sprintf("compacted %d→%d", r.TokensBefore, r.EstimatedTokensAfter)
}

func (u piUsage) zero() bool {
	return u.Input+u.Output+u.CacheRead+u.CacheWrite == 0
}

// usageEvery is the least gap between two live usage events; it matches the record's activity save interval.
var usageEvery = 2 * time.Second

// parseEvents turns Pi's JSONL into wire events; the exit code says nothing, so the stream alone decides failure.
func parseEvents(br *bufio.Reader, sink wire.Sink, onMeaningful func(), onEvent func(piEvent) error, logs ...wire.StreamLog) (res wire.Result, _ error) {
	t := &piTurn{open: map[string]string{}, sink: sink, lg: wire.PickLog(logs), res: &res, onMeaningful: onMeaningful, onEvent: onEvent}
	defer t.flushLog()
	tr, err := wire.DecodeEvents(br, t.lg, "decode pi json", true, nil, t.handle)
	res.Transport = tr
	if err != nil {
		res.Answer = t.answerText()
		return res, err
	}
	err = t.finish(false)
	res.Answer = t.answerText()
	return res, err
}

// handback is the typed `handback` tool call's arguments; the schema lives in the guard extension's ACTIONS.
type handback struct {
	Status  string   `json:"status"`
	Summary string   `json:"summary"`
	Unfixed []string `json:"unfixed"`
}

// handbackStatuses is the schema's status enum: the verdict words.
var handbackStatuses = []string{"DONE", "DONE_WITH_CONCERNS", "NEEDS_CONTEXT", "BLOCKED"}

// parseHandback decodes a call's args; ok is false for a malformed call or a status outside the enum.
func parseHandback(args json.RawMessage) (h handback, ok bool) {
	if json.Unmarshal(args, &h) != nil || !slices.Contains(handbackStatuses, h.Status) {
		return handback{}, false
	}
	return h, true
}

// block is the verdict line, the summary and the single UNFIXED line, built from the typed fields.
func (h handback) block() string {
	unfixed := "none"
	if items := slices.DeleteFunc(slices.Clone(h.Unfixed), func(s string) bool { return strings.TrimSpace(s) == "" }); len(items) > 0 {
		unfixed = strings.Join(items, "; ")
	}
	head := h.Status
	if s := strings.TrimSpace(h.Summary); s != "" {
		head += ": " + s
	}
	return head + "\nUNFIXED: " + unfixed
}

// answerText is the final text, then the typed handback block last, so its status and UNFIXED line decide.
func (t *piTurn) answerText() string {
	text := t.text()
	if t.handback == nil {
		return text
	}
	return strings.TrimSpace(text + "\n\n" + t.handback.block())
}

// piTurn is parseEvents' state across one run's events.
type piTurn struct {
	sink         wire.Sink
	lg           wire.StreamLog
	res          *wire.Result
	answer       []string // one entry per assistant message that spoke
	cur          strings.Builder
	logLine      strings.Builder
	think        strings.Builder // thinking deltas since the last thinking_end
	stop         string          // the last assistant message's stop reason; only the last one decides the run
	errMsg       string          // and its error message
	assistant    bool
	open         map[string]string   // running tool calls, id to write path
	calls        map[string]handback // handback calls awaiting their result, by tool call id
	handback     *handback           // the last handback call that succeeded
	inflight     piUsage             // the last non-zero usage of the message still streaming; zero mid-turn means unknown
	usageAt      time.Time
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
		return t.update(ev)
	case "tool_execution_start":
		t.flushLog()
		t.lg.Emit("tool", ev.ToolName)
		t.progressed = true
		t.open[ev.ToolCallID] = ev.writePath()
		t.noteHandback(ev)
		use := wire.ToolEvent(ev.ToolName, ev.ToolCallID)
		use.Text = ev.argSummary()
		if sofar := t.sofar(); sofar != (wire.Usage{}) {
			use.Usage = &sofar
		}
		return t.sink.Emit(use)
	case "tool_execution_end":
		return t.toolEnd(ev)
	case "auto_retry_start", "summarization_retry_scheduled", "compaction_start", "compaction_end", "auto_retry_end":
		return t.phase(ev, ts)
	case "message_end":
		return t.messageEnd(ev)
	case "agent_settled":
		// agent_end is not final: overflow compaction and retries continue after it; agent_settled ends the run
		t.aborted = ev.Aborted
		return t.finish(true)
	}
	return nil
}

func (t *piTurn) phase(ev piEvent, ts *wire.TransportTracker) error {
	t.flushLog()
	switch ev.Type {
	case "auto_retry_start", "summarization_retry_scheduled":
		ts.Retry(nil)
		return t.retry(ev)
	}
	out := phaseEvent(ev)
	t.lg.Emit("phase", out.Status+": "+out.Text)
	return t.sink.Emit(out)
}

func (t *piTurn) retry(ev piEvent) error {
	text := fmt.Sprintf("attempt %d/%d retry_in=%dms", ev.Attempt, ev.MaxAttempts, ev.DelayMs)
	t.lg.Emit("retry", text)
	return t.sink.Emit(wire.Event{Type: wire.EventRetry, Text: text})
}

// phaseEvent marks a compaction or a retry sequence starting, ending well ("running") or ending badly ("warning").
func phaseEvent(ev piEvent) wire.Event {
	out := wire.Event{Type: wire.EventProgress, Agent: "pi", Status: "running"}
	switch ev.Type {
	case "compaction_start":
		out.Status, out.Text = "compacting", ev.Reason
	case "compaction_end":
		out.Text = ev.compactedLine()
		if ev.Aborted {
			out.Status, out.Text = "warning", "compaction aborted"
		} else if ev.ErrorMessage != "" {
			out.Status, out.Text = "warning", ev.ErrorMessage
		}
	case "auto_retry_end":
		if ev.Success != nil && !*ev.Success || ev.FinalError != "" {
			out.Status, out.Text = "warning", cmp.Or(ev.FinalError, "retries failed")
		}
	}
	return out
}

// noteHandback keeps a well-formed handback call until its result says it ran.
func (t *piTurn) noteHandback(ev piEvent) {
	if ev.ToolName != "handback" {
		return
	}
	if h, ok := parseHandback(ev.Args); ok {
		if t.calls == nil {
			t.calls = map[string]handback{}
		}
		t.calls[ev.ToolCallID] = h
	}
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
	if h, ok := t.calls[ev.ToolCallID]; ok {
		delete(t.calls, ev.ToolCallID)
		if !ev.IsError {
			t.handback = &h
		}
	}
	t.lg.Emit("tool", ev.ToolName+": "+status)
	return t.sink.Emit(wire.Event{Type: wire.EventToolResult, Tool: &wire.ToolRef{Name: ev.ToolName, ID: ev.ToolCallID},
		Status: status, Text: ev.resultLine(), DurationMs: ev.durationMs()})
}

func (t *piTurn) update(ev piEvent) error {
	if err := t.delta(ev); err != nil {
		return err
	}
	var u piUsage
	if json.Unmarshal(ev.Usage, &u) != nil {
		return nil
	}
	return t.liveUsage(u)
}

// sofar is the run's usage through the last message_end plus the message still streaming.
func (t *piTurn) sofar() wire.Usage {
	return addUsage(t.res.Usage, t.inflight)
}

func addUsage(u wire.Usage, tu piUsage) wire.Usage {
	u.InputTokens += tu.Input
	u.CacheReadTokens += tu.CacheRead
	u.CacheCreationTokens += tu.CacheWrite
	u.OutputTokens += tu.Output
	u.CostUSD += tu.Cost.Total
	return u
}

// liveUsage keeps the streaming message's latest non-zero cumulative usage; it forwards one at most per usageEvery.
func (t *piTurn) liveUsage(u piUsage) error {
	if u.zero() {
		return nil
	}
	t.inflight = u
	if now := time.Now(); now.Sub(t.usageAt) >= usageEvery {
		t.usageAt = now
		sofar := t.sofar()
		return t.sink.Emit(wire.Event{Type: wire.EventUsage, Usage: &sofar})
	}
	return nil
}

// argSummary is the tool call's primary argument (command, pattern or path), else its compact args, on one line.
func (ev piEvent) argSummary() string {
	var a map[string]any
	if json.Unmarshal(ev.Args, &a) != nil {
		return ""
	}
	for _, k := range []string{"command", "pattern", "path", "file_path", "query", "url"} {
		if v, ok := a[k].(string); ok && v != "" {
			return wire.Summary(v)
		}
	}
	if len(a) == 0 {
		return ""
	}
	return wire.Summary(string(ev.Args))
}

// resultLine is the first line of the tool result's text; a failed call's text is its error.
func (ev piEvent) resultLine() string {
	var r struct {
		Content []struct{ Text string }
	}
	if json.Unmarshal(ev.Result, &r) != nil {
		var s string
		_ = json.Unmarshal(ev.Result, &s)
		return wire.FirstLine(s)
	}
	for _, c := range r.Content {
		if line := wire.FirstLine(c.Text); line != "" {
			return line
		}
	}
	return ""
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
	if ev.Update != nil && strings.HasPrefix(ev.Update.Type, "thinking_") {
		return t.thinking(ev.Update.Type, ev.Update.Delta, ev.Update.Content)
	}
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

// thinking gathers a reasoning block's deltas and emits it as one summary line at thinking_end.
func (t *piTurn) thinking(kind string, delta, content json.RawMessage) error {
	switch kind {
	case "thinking_delta":
		if d, err := deltaText(delta); err == nil {
			t.think.WriteString(d)
		}
	case "thinking_end":
		var whole string
		if json.Unmarshal(content, &whole) != nil || strings.TrimSpace(whole) == "" {
			whole = t.think.String()
		}
		t.think.Reset()
		if line := wire.Summary(whole); line != "" {
			return t.sink.Emit(wire.Event{Type: wire.EventThinking, Text: line})
		}
	}
	return nil
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
	t.res.Usage = addUsage(t.res.Usage, m.Usage) // message_end's usage is the message's total: replaces inflight
	t.inflight = piUsage{}
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
