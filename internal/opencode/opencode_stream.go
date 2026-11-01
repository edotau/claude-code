package opencode

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/edotau/claude-code/internal/agent"
	"strings"
)

type openCodeEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionID"`
	Part      struct {
		Text   string `json:"text"`
		Tool   string `json:"tool"`
		CallID string `json:"callID"`
		State  struct {
			Status string `json:"status"`
		} `json:"state"`
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

// isMeaningfulOpenCodeEvent reports whether ev is real model output, not bookkeeping (step_start) or an empty part.
func isMeaningfulOpenCodeEvent(ev openCodeEvent) bool {
	switch ev.Type {
	case "text", "reasoning":
		if strings.TrimSpace(ev.Part.Text) != "" {
			return true
		}
	case "tool":
		if ev.Part.Tool != "" || ev.Part.State.Status != "" {
			return true
		}
	}
	return ev.Part.Tokens.Output > 0
}

// parseOpenCodeStream streams the root session's text parts; onMeaningful fires once on the first meaningful event.
func parseOpenCodeStream(br *bufio.Reader, sink agent.Sink, onMeaningful func(), logs ...agent.StreamLog) (res agent.Result, _ error) {
	ts := agent.TransportTracker{Start: agent.Clock()}
	defer func() { res.Transport = ts.Result() }()
	lg := agent.PickLog(logs)
	var texts, errs []string
	seenTools := map[string]bool{}
	fired := false
	err := agent.DecodeLines(br, lg, func(raw []byte) (bool, error) {
		var ev openCodeEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			return false, fmt.Errorf("decode opencode json: %w", err)
		}
		ts.Event(agent.Clock())
		if ev.SessionID != "" {
			res.SessionID = ev.SessionID
		}
		if !fired && onMeaningful != nil && isMeaningfulOpenCodeEvent(ev) {
			fired = true
			onMeaningful()
		}
		switch ev.Type {
		case "text":
			if ev.Part.Text == "" {
				return false, nil
			}
			if err := sink.Emit(agent.Event{Type: agent.EventText, Text: ev.Part.Text, Sep: len(texts) > 0}); err != nil {
				return false, err
			}
			texts = append(texts, ev.Part.Text)
			lg.Emit("assistant", ev.Part.Text)
		case "tool", "tool_use":
			if err := emitOpenCodeTool(sink, ev, seenTools); err != nil {
				return false, err
			}
		case "step_finish":
			t := ev.Part.Tokens
			res.Usage.InputTokens += t.Input
			res.Usage.CacheReadTokens += t.Cache.Read
			res.Usage.CacheCreationTokens += t.Cache.Write
			res.Usage.OutputTokens += t.Output + t.Reasoning
			lg.Emit("result", fmt.Sprintf("step finished, tokens in=%d out=%d", t.Input, t.Output+t.Reasoning))
		case "error":
			msg := cmp.Or(ev.Error.Data.Message, ev.Error.Name, "unknown error")
			errs = append(errs, msg)
			if err := agent.EmitError(lg, sink, msg); err != nil {
				return false, err
			}
		}
		return false, nil
	})
	if err != nil {
		return res, err
	}
	res.Answer = strings.TrimSpace(strings.Join(texts, "\n"))
	if len(errs) > 0 {
		return res, errors.New(strings.Join(errs, "; "))
	}
	return res, nil
}

// emitOpenCodeTool emits tool_use once per call id, then tool_result when that call completes or errors.
func emitOpenCodeTool(sink agent.Sink, ev openCodeEvent, seen map[string]bool) error {
	p := ev.Part
	if p.Tool == "" {
		return nil
	}
	if !seen[p.CallID] {
		seen[p.CallID] = true
		if err := sink.Emit(agent.ToolEvent(p.Tool, p.CallID)); err != nil {
			return err
		}
	}
	if p.State.Status == "completed" || p.State.Status == "error" {
		return sink.Emit(agent.Event{Type: agent.EventToolResult, Tool: &agent.ToolRef{Name: p.Tool, ID: p.CallID}, Status: p.State.Status})
	}
	return nil
}
