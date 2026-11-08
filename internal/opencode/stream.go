package opencode

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
)

type openCodeEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionID"`
	Part      struct {
		Text   string `json:"text"`
		Tool   string `json:"tool"`
		CallID string `json:"callID"`
		State  struct {
			Status string         `json:"status"`
			Input  map[string]any `json:"input"`  // the call's arguments, sent once the tool has run
			Output any            `json:"output"` // its result (a string for the built-in tools)
		} `json:"state"`
		Reason string  `json:"reason"` // step_finish: stop, length, tool-calls, ...
		Cost   float64 `json:"cost"`   // step_finish: USD
		Tokens struct {
			Input, Output, Reasoning int
			Cache                    struct{ Read, Write int }
		} `json:"tokens"`
	} `json:"part"`
	Error struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
}

// isMeaningfulOpenCodeEvent reports whether ev is real model output; step_start is not (it only renews the watchdog).
func isMeaningfulOpenCodeEvent(ev openCodeEvent) bool {
	switch ev.Type {
	case "step_finish":
		return true
	case "text", "reasoning":
		if strings.TrimSpace(ev.Part.Text) != "" {
			return true
		}
	case "tool", "tool_use":
		if ev.Part.Tool != "" || ev.Part.State.Status != "" {
			return true
		}
	}
	return ev.Part.Tokens.Output > 0
}

// parseOpenCodeEvents streams the root session's text parts; onMeaningful fires once on the first meaningful event,
// onTool once per finished tool call, onStep on every step_start before that first meaningful event.
func parseOpenCodeEvents(br *bufio.Reader, sink wire.Sink, onMeaningful func(), onTool func(openCodeEvent), onStep func(), logs ...wire.StreamLog) (res wire.Result, _ error) {
	ts := wire.TransportTracker{Start: wire.Clock()}
	defer func() { res.Transport = ts.Result() }()
	lg := wire.PickLog(logs)
	t := &ocTurn{sink: sink, lg: lg, res: &res, ts: &ts, ans: wire.Answer{Join: "\n"}, seenTools: map[string]bool{},
		onMeaningful: onMeaningful, onTool: onTool, onStep: onStep}
	err := wire.DecodeLines(br, lg, t.handle)
	res.Answer = t.ans.String()
	if err != nil {
		return res, err
	}
	if len(t.errs) > 0 {
		return res, errors.New(strings.Join(t.errs, "; "))
	}
	return res, nil
}

// ocTurn is parseOpenCodeEvents' state across one run's JSON events.
type ocTurn struct {
	sink         wire.Sink
	lg           wire.StreamLog
	res          *wire.Result
	ts           *wire.TransportTracker
	ans          wire.Answer
	errs         []string
	seenTools    map[string]bool
	fired        bool
	onMeaningful func()
	onTool       func(openCodeEvent)
	onStep       func()
}

// handle decodes one line and applies it.
func (t *ocTurn) handle(raw []byte) (bool, error) {
	var ev openCodeEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return false, fmt.Errorf("decode opencode json: %w", err)
	}
	t.ts.Event(wire.Clock())
	if ev.SessionID != "" {
		t.res.SessionID = ev.SessionID
	}
	t.notify(ev)
	return false, t.apply(ev)
}

// notify fires the meaningful callback once, and the step callback on step_start until then.
func (t *ocTurn) notify(ev openCodeEvent) {
	if !t.fired && t.onMeaningful != nil && isMeaningfulOpenCodeEvent(ev) {
		t.fired = true
		t.onMeaningful()
	}
	if !t.fired && t.onStep != nil && ev.Type == "step_start" {
		t.onStep()
	}
}

func (t *ocTurn) apply(ev openCodeEvent) error {
	switch ev.Type {
	case "text":
		return t.text(ev)
	case "tool", "tool_use":
		return t.tool(ev)
	case "step_finish":
		t.stepFinish(ev)
	case "error":
		return t.errorEvent(ev)
	}
	return nil
}

func (t *ocTurn) text(ev openCodeEvent) error {
	if ev.Part.Text == "" {
		return nil
	}
	if err := t.ans.Emit(t.sink, ev.Part.Text, true); err != nil {
		return err
	}
	t.lg.Emit("assistant", ev.Part.Text)
	return nil
}

func (t *ocTurn) tool(ev openCodeEvent) error {
	if err := emitOpenCodeTool(t.sink, ev, t.seenTools); err != nil {
		return err
	}
	if t.onTool != nil && ev.Part.State.Status == "completed" {
		t.onTool(ev)
	}
	return nil
}

func (t *ocTurn) stepFinish(ev openCodeEvent) {
	tk := ev.Part.Tokens
	u := &t.res.Usage
	u.InputTokens += tk.Input
	u.CacheReadTokens += tk.Cache.Read
	u.CacheCreationTokens += tk.Cache.Write
	u.OutputTokens += tk.Output + tk.Reasoning
	u.CostUSD += ev.Part.Cost
	t.lg.Emit("result", fmt.Sprintf("step finished (%s), tokens in=%d out=%d", cmp.Or(ev.Part.Reason, "unknown"), tk.Input, tk.Output+tk.Reasoning))
}

func (t *ocTurn) errorEvent(ev openCodeEvent) error {
	msg := cmp.Or(ev.Error.Data.Message, ev.Error.Name, "unknown error")
	t.errs = append(t.errs, msg)
	return wire.EmitError(t.lg, t.sink, msg)
}

// emitOpenCodeTool emits tool_use once per call id, then tool_result when that call completes or errors.
func emitOpenCodeTool(sink wire.Sink, ev openCodeEvent, seen map[string]bool) error {
	p := ev.Part
	if p.Tool == "" {
		return nil
	}
	if !seen[p.CallID] {
		seen[p.CallID] = true
		if err := sink.Emit(wire.ToolEvent(p.Tool, p.CallID)); err != nil {
			return err
		}
	}
	if p.State.Status == "completed" || p.State.Status == "error" {
		return sink.Emit(wire.Event{Type: wire.EventToolResult, Tool: &wire.ToolRef{Name: p.Tool, ID: p.CallID}, Status: p.State.Status})
	}
	return nil
}

// toolCallMarkupRE matches tool-call markup a model writes as text when it has no tools to call.
var toolCallMarkupRE = regexp.MustCompile(`(?s)<(invoke|function_calls|antml:invoke)\b.*?(</(invoke|function_calls|antml:invoke)>|$)|\b_?tool_use\b`)

// refuseToolCallText fails a tools-off run whose whole answer is tool-call markup: the model tried to act, nothing ran.
func refuseToolCallText(name string, req agent.RunOptions, answer string) error {
	if (req.Tools && !req.ReadOnly) || !toolCallMarkupRE.MatchString(answer) {
		return nil
	}
	if rest := strings.TrimSpace(toolCallMarkupRE.ReplaceAllString(answer, "")); len(rest) > 40 {
		return nil // prose around the markup is a real answer that quotes it
	}
	return &retry.Error{Kind: retry.KindFatal, Agent: name, Err: errors.New(
		"the model answered with tool calls, but tools are off so none ran; re-run with --tools or use another agent")}
}
