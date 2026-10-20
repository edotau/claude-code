package translate

import (
	"bufio"
	"bytes"
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
	buf       bytes.Buffer // reused by event() to avoid a per-event json.Marshal allocation
}

// Typed event payloads replace map[string]any: struct marshaling skips the reflect-based key
// sort and per-value interface boxing a map incurs, the dominant cost on the per-chunk hot path.

type textBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type blockStartEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock any    `json:"content_block"`
}

type textDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type inputJSONDelta struct {
	Type        string `json:"type"`
	PartialJSON string `json:"partial_json"`
}

type blockDeltaEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta any    `json:"delta"`
}

type blockStopEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type startUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type messageStartMsg struct {
	ID           string     `json:"id"`
	Type         string     `json:"type"`
	Role         string     `json:"role"`
	Model        string     `json:"model"`
	Content      []any      `json:"content"`
	StopReason   any        `json:"stop_reason"`
	StopSequence any        `json:"stop_sequence"`
	Usage        startUsage `json:"usage"`
}

type messageStartEvent struct {
	Type    string          `json:"type"`
	Message messageStartMsg `json:"message"`
}

type messageDeltaInner struct {
	StopReason   string `json:"stop_reason"`
	StopSequence any    `json:"stop_sequence"`
}

type messageDeltaEvent struct {
	Type  string            `json:"type"`
	Delta messageDeltaInner `json:"delta"`
	Usage any               `json:"usage"`
}

type messageStopEvent struct {
	Type string `json:"type"`
}

// StreamChat converts a chat-completions SSE stream into Anthropic Messages SSE events, calling flush per event.
// A read error before any output returns without writing so the caller may still fail over.
func StreamChat(w io.Writer, flush func(), r io.Reader, clientModel string) error {
	s := &streamer{w: w, flush: flush, model: clientModel, open: -1, tools: map[int]*toolState{}}
	br := bufio.NewReader(r)
	var data [][]byte // SSE "data:" lines for the frame in progress; each a fresh slice from ReadBytes
	dispatch := func() (bool, error) {
		if len(data) == 0 {
			return false, nil
		}
		payload := bytes.Join(data, []byte("\n"))
		data = data[:0]
		return s.handle(payload)
	}
	for {
		line, readErr := br.ReadBytes('\n')
		line = bytes.TrimRight(line, "\r\n")
		switch {
		case len(line) == 0:
			if done, err := dispatch(); err != nil || done {
				return s.end(err)
			}
		case bytes.HasPrefix(line, []byte("data:")):
			data = append(data, bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" ")))
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
func (s *streamer) handle(payload []byte) (bool, error) {
	if string(bytes.TrimSpace(payload)) == "[DONE]" {
		return true, nil
	}
	var c streamChunk
	if err := json.Unmarshal(payload, &c); err != nil {
		return false, s.fail(500, "malformed upstream stream chunk", err)
	}
	if len(c.Error) > 0 && string(c.Error) != "null" {
		body := FromChatError(streamErrorStatus(c.Error), payload)
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
		if err := s.event("content_block_start", blockStartEvent{Type: "content_block_start", Index: s.next,
			ContentBlock: textBlock{Type: "text", Text: ""}}); err != nil {
			return err
		}
		s.open, s.openText = s.next, true
		s.next++
	}
	return s.delta(textDelta{Type: "text_delta", Text: t})
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
		if err := s.event("content_block_start", blockStartEvent{Type: "content_block_start", Index: s.next,
			ContentBlock: toolBlock{Type: "tool_use", ID: t.id, Name: t.name, Input: json.RawMessage("{}")}}); err != nil {
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
	return s.delta(inputJSONDelta{Type: "input_json_delta", PartialJSON: args})
}

func (s *streamer) delta(d any) error {
	return s.event("content_block_delta", blockDeltaEvent{Type: "content_block_delta", Index: s.open, Delta: d})
}

func (s *streamer) closeBlock() error {
	if s.open < 0 {
		return nil
	}
	err := s.event("content_block_stop", blockStopEvent{Type: "content_block_stop", Index: s.open})
	s.open, s.openText, s.openTool = -1, false, nil
	return err
}

func (s *streamer) start() error {
	if s.started {
		return nil
	}
	s.started = true
	return s.event("message_start", messageStartEvent{Type: "message_start", Message: messageStartMsg{
		ID: newID("msg_"), Type: "message", Role: "assistant", Model: s.model, Content: []any{},
		StopReason: nil, StopSequence: nil, Usage: startUsage{InputTokens: 0, OutputTokens: 0},
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
	var usage any = map[string]any{"output_tokens": 0}
	if s.usage != nil {
		usage = anthropicUsage(s.usage)
	}
	if err := s.event("message_delta", messageDeltaEvent{Type: "message_delta",
		Delta: messageDeltaInner{StopReason: stopReason(s.finish, s.usedTools), StopSequence: nil}, Usage: usage}); err != nil {
		return err
	}
	return s.event("message_stop", messageStopEvent{Type: "message_stop"})
}

// fail writes an Anthropic error event and returns cause wrapped.
func (s *streamer) fail(status int, msg string, cause error) error {
	body, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": errorType(status), "message": msg}})
	if err := s.event("error", json.RawMessage(body)); err != nil {
		return err
	}
	return fmt.Errorf("translate: %s: %w", msg, cause)
}

// event marshals payload into s.buf (reused across calls, unlike json.Marshal's fresh allocation
// each time) and writes one SSE frame.
func (s *streamer) event(typ string, payload any) error {
	s.buf.Reset()
	if err := json.NewEncoder(&s.buf).Encode(payload); err != nil {
		return err
	}
	b := bytes.TrimRight(s.buf.Bytes(), "\n")
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", typ, b); err != nil {
		return err
	}
	if s.flush != nil {
		s.flush()
	}
	return nil
}
