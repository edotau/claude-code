package translate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
)

type streamChoice struct {
	Delta struct {
		Content   json.RawMessage `json:"content"`
		Refusal   string          `json:"refusal"`
		ToolCalls []inToolCall    `json:"tool_calls"`
	} `json:"delta"`
	FinishReason string `json:"finish_reason"`
}

type streamChunk struct {
	Choices []streamChoice  `json:"choices"`
	Usage   *chatUsage      `json:"usage"`
	Error   json.RawMessage `json:"error"`
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
	next      int          // next Anthropic content block index
	open      int          // index of the open text block, -1 when none
	openText  bool         // the open block is text
	opened    []*toolState // started tool blocks, stopped together at stream end
	tools     map[int]*toolState
	calls     []*toolState // every tool call seen, to catch one whose name never arrived
	last      *toolState
	usedTools bool
	finish    string
	usage     *chatUsage
	buf       bytes.Buffer // reused by event() to avoid a per-event json.Marshal allocation
}

// textBlock is a text content block or text_delta; typed payloads skip map[string]any's per-chunk marshal cost.
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

// StreamChat converts chat-completions SSE to Anthropic SSE; a read error before any output writes nothing.
func StreamChat(w io.Writer, flush func(), r io.Reader, clientModel string) error {
	s := &streamer{w: w, flush: flush, model: clientModel, open: -1, tools: map[int]*toolState{}}
	br := bufio.NewReader(r)
	var f sseFrame
	for {
		line, readErr := readSSELine(br)
		if done, err := s.feed(&f, bytes.TrimRight(line, "\r\n")); err != nil || done {
			return s.end(err)
		}
		if readErr != nil {
			return s.readEnd(&f, readErr)
		}
	}
}

// maxSSELine caps one upstream SSE line; a longer one is an interrupted stream, not unbounded memory.
const maxSSELine = 32 << 20

var errSSELineTooLong = errors.New("upstream SSE line exceeds limit")

// readSSELine is br.ReadBytes('\n') that fails with errSSELineTooLong past maxSSELine.
func readSSELine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if line = append(line, chunk...); len(line) > maxSSELine {
			return nil, errSSELineTooLong
		}
		if err != bufio.ErrBufferFull {
			return line, err
		}
	}
}

// sseFrame is the SSE frame in progress: its event name and "data:" lines, each a fresh slice from ReadBytes.
type sseFrame struct {
	event string
	data  [][]byte
}

// feed adds one SSE line to f, dispatching the frame on a blank line.
func (s *streamer) feed(f *sseFrame, line []byte) (bool, error) {
	switch {
	case len(line) == 0:
		return s.dispatch(f)
	case bytes.HasPrefix(line, []byte("data:")):
		f.data = append(f.data, bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" ")))
	case bytes.HasPrefix(line, []byte("event:")):
		f.event = string(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("event:"))))
	}
	return false, nil
}

// dispatch handles f's payload, if any, and resets f.
func (s *streamer) dispatch(f *sseFrame) (bool, error) {
	if len(f.data) == 0 {
		f.event = ""
		return false, nil
	}
	payload, event := bytes.Join(f.data, []byte("\n")), f.event
	f.data, f.event = f.data[:0], ""
	return s.handle(event, payload)
}

// readEnd finishes the stream on a read error: EOF dispatches the last frame, anything else is an interruption.
func (s *streamer) readEnd(f *sseFrame, readErr error) error {
	if readErr == io.EOF {
		done, err := s.dispatch(f)
		return s.end(s.truncated(done, err))
	}
	if !s.started {
		return fmt.Errorf("translate: stream read: %w", readErr)
	}
	return s.end(s.fail("upstream stream interrupted: "+readErr.Error(), readErr))
}

// handle processes one SSE payload and reports whether the stream is done.
func (s *streamer) handle(event string, payload []byte) (bool, error) {
	if string(bytes.TrimSpace(payload)) == "[DONE]" {
		return true, nil
	}
	c, err := s.parseChunk(event, payload)
	if err != nil {
		return false, err
	}
	if err := s.start(); err != nil {
		return false, err
	}
	if c.Usage != nil {
		s.usage = c.Usage
	}
	for _, ch := range c.Choices[:min(len(c.Choices), 1)] {
		if err := s.choice(ch); err != nil {
			return false, err
		}
	}
	return false, nil
}

// parseChunk decodes one payload; an in-band error, a bare `event: error` frame or bad JSON writes an error event.
func (s *streamer) parseChunk(event string, payload []byte) (streamChunk, error) {
	var c streamChunk
	err := json.Unmarshal(payload, &c)
	switch {
	case err == nil && len(c.Error) > 0 && string(c.Error) != "null":
		return c, s.upstreamError(c.Error, payload)
	case event == "error":
		return c, s.upstreamError(payload, payload)
	case err != nil:
		return c, s.fail("malformed upstream stream chunk", err)
	}
	return c, nil
}

// upstreamError writes an in-band upstream error as an error event and returns it as an error.
func (s *streamer) upstreamError(raw json.RawMessage, payload []byte) error {
	body := FromChatError(streamErrorStatus(raw), payload)
	if err := s.event("error", json.RawMessage(body)); err != nil {
		return err
	}
	return fmt.Errorf("translate: upstream stream error: %s", raw)
}

// choice emits the text and tool-call deltas of one stream choice and records its finish reason.
func (s *streamer) choice(ch streamChoice) error {
	text, err := contentText(ch.Delta.Content)
	if err != nil {
		return s.fail("malformed upstream content", err)
	}
	if text += ch.Delta.Refusal; text != "" {
		if err := s.text(text); err != nil {
			return err
		}
	}
	for _, tc := range ch.Delta.ToolCalls {
		if err := s.tool(tc); err != nil {
			return err
		}
	}
	if ch.FinishReason != "" {
		s.finish = ch.FinishReason
	}
	return nil
}

func (s *streamer) text(t string) error {
	if !s.openText {
		if err := s.event("content_block_start", blockStartEvent{Type: "content_block_start", Index: s.next,
			ContentBlock: textBlock{Type: "text", Text: ""}}); err != nil {
			return err
		}
		s.open, s.openText = s.next, true
		s.next++
	}
	return s.delta(s.open, textBlock{Type: "text_delta", Text: t})
}

func (s *streamer) tool(tc inToolCall) error {
	t := s.toolFor(tc)
	s.last = t
	if t.id == "" {
		t.id = tc.ID
	}
	if t.name == "" {
		t.name = tc.Function.Name
	}
	args := argsText(tc.Function.Arguments)
	if t.block < 0 {
		var started bool
		var err error
		if args, started, err = s.startTool(t, args); err != nil || !started {
			return err
		}
	}
	if args == "" {
		return nil
	}
	return s.delta(t.block, inputJSONDelta{Type: "input_json_delta", PartialJSON: args})
}

// toolFor finds the tool call tc continues, or registers a new one.
func (s *streamer) toolFor(tc inToolCall) *toolState {
	var t *toolState
	if tc.Index != nil {
		t = s.tools[*tc.Index]
	} else {
		t = s.last
	}
	if startsNewCall(tc, t) {
		t = &toolState{block: -1}
		s.calls = append(s.calls, t)
		if tc.Index != nil {
			s.tools[*tc.Index] = t
		}
	}
	return t
}

// startsNewCall reports whether tc begins a call other than t: none yet, a new id, or an index-less fresh name.
func startsNewCall(tc inToolCall, t *toolState) bool {
	if t == nil || (tc.ID != "" && t.id != "" && tc.ID != t.id) {
		return true
	}
	// without an index, a name on a named call starts the next one unless an id says it is the same function
	return tc.Index == nil && t.name != "" && tc.Function.Name != "" && (tc.ID == "" || tc.Function.Name != t.name)
}

// startTool opens t's tool_use block once named, returning buffered args joined with args; until then started is false.
func (s *streamer) startTool(t *toolState, args string) (full string, started bool, err error) {
	if t.name == "" {
		t.pending.WriteString(args)
		return "", false, nil
	}
	if err := s.closeText(); err != nil {
		return "", false, err
	}
	id := t.id // a synthesized id stays off t, so a real id arriving after the name continues this call
	if id == "" {
		id = newID("toolu_")
	}
	if err := s.event("content_block_start", blockStartEvent{Type: "content_block_start", Index: s.next,
		ContentBlock: toolBlock{Type: "tool_use", ID: id, Name: t.name, Input: json.RawMessage("{}")}}); err != nil {
		return "", false, err
	}
	t.block, s.usedTools = s.next, true
	s.opened = append(s.opened, t)
	s.next++
	full = t.pending.String() + args
	t.pending.Reset()
	return full, true, nil
}

func (s *streamer) delta(index int, d any) error {
	return s.event("content_block_delta", blockDeltaEvent{Type: "content_block_delta", Index: index, Delta: d})
}

func (s *streamer) closeText() error {
	if !s.openText {
		return nil
	}
	err := s.event("content_block_stop", blockStopEvent{Type: "content_block_stop", Index: s.open})
	s.open, s.openText = -1, false
	return err
}

// closeTools stops every started tool block; they stay open so interleaved argument fragments route by index.
func (s *streamer) closeTools() error {
	for _, t := range s.opened {
		if err := s.event("content_block_stop", blockStopEvent{Type: "content_block_stop", Index: t.block}); err != nil {
			return err
		}
	}
	s.opened = nil
	return nil
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

// truncated turns a clean EOF with neither [DONE] nor finish_reason into a stream error; an empty body writes nothing.
func (s *streamer) truncated(done bool, err error) error {
	if err != nil || done || s.finish != "" {
		return err
	}
	if !s.started {
		return fmt.Errorf("translate: stream read: %w", io.ErrUnexpectedEOF)
	}
	return s.fail("upstream stream ended without a finish_reason", io.ErrUnexpectedEOF)
}

// end closes the message unless err is set; an error event has already been written for in-band failures.
func (s *streamer) end(err error) error {
	if err != nil {
		return err
	}
	if err := s.start(); err != nil {
		return err
	}
	if slices.ContainsFunc(s.calls, func(t *toolState) bool { return t.block < 0 }) {
		return s.fail("upstream tool call never named its function", io.ErrUnexpectedEOF)
	}
	if err := s.closeText(); err != nil {
		return err
	}
	if err := s.closeTools(); err != nil {
		return err
	}
	usage := map[string]any{"output_tokens": 0}
	if s.usage != nil {
		usage = anthropicUsage(s.usage)
	}
	delta := messageDeltaInner{StopReason: stopReason(s.finish, s.usedTools), StopSequence: nil}
	if err := s.event("message_delta", messageDeltaEvent{Type: "message_delta", Delta: delta, Usage: usage}); err != nil {
		return err
	}
	return s.event("message_stop", messageStopEvent{Type: "message_stop"})
}

// fail writes an Anthropic api_error event and returns cause wrapped.
func (s *streamer) fail(msg string, cause error) error {
	if err := s.event("error", json.RawMessage(ErrorBody(http.StatusInternalServerError, msg))); err != nil {
		return err
	}
	return fmt.Errorf("translate: %s: %w", msg, cause)
}

// event marshals payload into the reused s.buf (no per-call json.Marshal allocation) and writes one SSE frame.
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
