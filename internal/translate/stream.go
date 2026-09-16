package translate

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string       `json:"content"`
			Refusal   string       `json:"refusal"`
			ToolCalls []inToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage      `json:"usage"`
	Error json.RawMessage `json:"error"`
}

type toolState struct {
	id, name string
	pending  strings.Builder // argument fragments seen before the name arrived
	block    int             // Anthropic content block index, -1 until started
}

type streamer struct {
	w         io.Writer
	flush     func()
	model     string
	started   bool
	next      int        // next Anthropic content block index
	open      int        // index of the open block, -1 when none
	openText  bool       // the open block is text
	openTool  *toolState // the open block is this tool call
	tools     map[int]*toolState
	last      *toolState
	usedTools bool
	finish    string
	usage     *chatUsage
}

// StreamChat converts a chat-completions SSE stream into Anthropic Messages SSE events, calling flush per event.
// A read error before any output returns without writing so the caller may still fail over.
func StreamChat(w io.Writer, flush func(), r io.Reader, clientModel string) error {
	s := &streamer{w: w, flush: flush, model: clientModel, open: -1, tools: map[int]*toolState{}}
	br := bufio.NewReader(r)
	var data []string
	dispatch := func() (bool, error) {
		if len(data) == 0 {
			return false, nil
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		return s.handle(payload)
	}
	for {
		line, readErr := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if done, err := dispatch(); err != nil || done {
				return s.end(err)
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		if readErr == io.EOF {
			_, err := dispatch()
			return s.end(err)
		}
		if readErr != nil {
			if !s.started {
				return fmt.Errorf("translate: stream read: %w", readErr)
			}
			return s.end(s.fail(500, "upstream stream interrupted: "+readErr.Error(), readErr))
		}
	}
}

var errUpstream = errors.New("translate: upstream stream error")

// handle processes one SSE payload and reports whether the stream is done.
func (s *streamer) handle(payload string) (bool, error) {
	if strings.TrimSpace(payload) == "[DONE]" {
		return true, nil
	}
	var c streamChunk
	if err := json.Unmarshal([]byte(payload), &c); err != nil {
		return false, s.fail(500, "malformed upstream stream chunk", err)
	}
	if len(c.Error) > 0 && string(c.Error) != "null" {
		body := FromChatError(streamErrorStatus(c.Error), []byte(payload))
		if err := s.event("error", json.RawMessage(body)); err != nil {
			return false, err
		}
		return false, fmt.Errorf("%w: %s", errUpstream, c.Error)
	}
	if err := s.start(); err != nil {
		return false, err
	}
	if c.Usage != nil {
		s.usage = c.Usage
	}
	for _, ch := range c.Choices[:min(len(c.Choices), 1)] {
		if text := ch.Delta.Content + ch.Delta.Refusal; text != "" {
			if err := s.text(text); err != nil {
				return false, err
			}
		}
		for _, tc := range ch.Delta.ToolCalls {
			if err := s.tool(tc); err != nil {
				return false, err
			}
		}
		if ch.FinishReason != "" {
			s.finish = ch.FinishReason
		}
	}
	return false, nil
}

func (s *streamer) text(t string) error {
	if !s.openText {
		if err := s.closeBlock(); err != nil {
			return err
		}
		if err := s.event("content_block_start", map[string]any{"type": "content_block_start", "index": s.next,
			"content_block": map[string]any{"type": "text", "text": ""}}); err != nil {
			return err
		}
		s.open, s.openText = s.next, true
		s.next++
	}
	return s.delta(map[string]any{"type": "text_delta", "text": t})
}

func (s *streamer) tool(tc inToolCall) error {
	var t *toolState
	if tc.Index != nil {
		t = s.tools[*tc.Index]
	} else {
		t = s.last
	}
	if t == nil || (tc.ID != "" && t.id != "" && tc.ID != t.id) {
		t = &toolState{block: -1}
		if tc.Index != nil {
			s.tools[*tc.Index] = t
		}
	}
	s.last = t
	if t.id == "" {
		t.id = tc.ID
	}
	if t.name == "" {
		t.name = tc.Function.Name
	}
	args := argsText(tc.Function.Arguments)
	if t.block < 0 {
		if t.name == "" {
			t.pending.WriteString(args)
			return nil
		}
		if err := s.closeBlock(); err != nil {
			return err
		}
		if t.id == "" {
			t.id = newID("toolu_")
		}
		if err := s.event("content_block_start", map[string]any{"type": "content_block_start", "index": s.next,
			"content_block": map[string]any{"type": "tool_use", "id": t.id, "name": t.name, "input": map[string]any{}}}); err != nil {
			return err
		}
		t.block, s.open, s.openTool, s.usedTools = s.next, s.next, t, true
		s.next++
		args = t.pending.String() + args
		t.pending.Reset()
	} else if s.openTool != t {
		return nil // fragment for an already-closed block: Anthropic blocks cannot be reopened
	}
	if args == "" {
		return nil
	}
	return s.delta(map[string]any{"type": "input_json_delta", "partial_json": args})
}

func (s *streamer) delta(d map[string]any) error {
	return s.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": s.open, "delta": d})
}

func (s *streamer) closeBlock() error {
	if s.open < 0 {
		return nil
	}
	err := s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.open})
	s.open, s.openText, s.openTool = -1, false, nil
	return err
}

func (s *streamer) start() error {
	if s.started {
		return nil
	}
	s.started = true
	return s.event("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": newID("msg_"), "type": "message", "role": "assistant", "model": s.model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
	}})
}

// end closes the message unless err is set; an error event has already been written for in-band failures.
func (s *streamer) end(err error) error {
	if err != nil {
		return err
	}
	if err := s.start(); err != nil {
		return err
	}
	if err := s.closeBlock(); err != nil {
		return err
	}
	usage := map[string]any{"output_tokens": 0}
	if s.usage != nil {
		usage = anthropicUsage(s.usage)
	}
	if err := s.event("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": stopReason(s.finish, s.usedTools), "stop_sequence": nil}, "usage": usage}); err != nil {
		return err
	}
	return s.event("message_stop", map[string]any{"type": "message_stop"})
}

// fail writes an Anthropic error event and returns cause wrapped.
func (s *streamer) fail(status int, msg string, cause error) error {
	body, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": errorType(status), "message": msg}})
	if err := s.event("error", json.RawMessage(body)); err != nil {
		return err
	}
	return fmt.Errorf("translate: %s: %w", msg, cause)
}

func (s *streamer) event(typ string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", typ, b); err != nil {
		return err
	}
	if s.flush != nil {
		s.flush()
	}
	return nil
}
