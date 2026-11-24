// detail.go is the dashboard's detail pane: one job's event stream, one clipped line per event, followed while live.

package tree

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/ansi"
)

// streamFunc writes a job's event lines to w until the job settles or ctx ends.
type streamFunc func(ctx context.Context, rec agent.Record, w io.Writer) error

// paneStream is what `agents result -f --events` streams: a carrier's children's events, tagged via, then its own.
var paneStream streamFunc = ask.StreamEvents

// paneLog follows the job's own log as `agents result -f <id>` does: one read from byte 0, then only new bytes.
var paneLog streamFunc = func(ctx context.Context, rec agent.Record, w io.Writer) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if _, err = agent.Tail(ctx, rec.Agent, cwd, rec.ID, true, w); err != nil && ctx.Err() == nil {
		_, _ = io.WriteString(w, "error: "+err.Error()+"\n")
	}
	return nil
}

// tailKeep caps the pane's kept lines and its feed's ring of unread ones.
const tailKeep = 500

// eventLine is one event line rendered for the pane; false for a line the pane does not show.
func eventLine(raw []byte, width int) (string, bool) {
	var ev wire.Event
	if json.Unmarshal(raw, &ev) != nil {
		return "", false
	}
	return paneEventLine(ev, width)
}

func paneEventLine(ev wire.Event, width int) (string, bool) {
	if ev.Type == "" || ev.Type == "heartbeat" || ev.Type == wire.EventProgress {
		return "", false
	}
	var parts []string
	if ev.Via != "" {
		parts = append(parts, viaTag(ev))
	}
	parts = append(parts, eventParts(ev)...)
	return textutil.TruncateCells(paneText(strings.Join(parts, " ")), min(max(width, 0), maxLineWidth), "…"), true
}

// eventParts is an event's words: a marked tool call or result, thinking, else type, tool, status and text.
func eventParts(ev wire.Event) []string {
	var parts []string
	name := ""
	if ev.Tool != nil {
		name = ev.Tool.Name
	}
	switch ev.Type {
	case wire.EventToolUse:
		parts = append(parts, "▸", name)
	case wire.EventToolResult:
		mark, status := "✓", ev.Status
		switch status {
		case "error":
			mark = "✗"
		case "completed":
			status = ""
		}
		parts = append(parts, mark, name, status)
	case eventThinking:
		parts = append(parts, "∴")
	case wire.EventText:
	default:
		parts = append(parts, ev.Type, name, ev.Status)
	}
	parts = append(parts, ev.Text)
	return slices.DeleteFunc(parts, func(p string) bool { return p == "" })
}

// viaTag names whose event a carrier forwarded: the child's agent and its id's clock (ids share their date prefix).
func viaTag(ev wire.Event) string {
	id := ev.Via
	agentName := cmp.Or(ev.Agent, id[strings.LastIndexByte(id, '-')+1:])
	clock := treeShortID(id)
	if _, rest, ok := strings.Cut(id, "T"); ok {
		clock, _, _ = strings.Cut(rest, ".")
	}
	return "↳ " + agentName + "·" + clock
}

// escSeq matches CSI, OSC and two-byte escapes; ansi.Strip removes only the kit's own style markers.
var escSeq = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)?|\x1b[@-_]`)

// stripEsc removes escape sequences before a caller drops the bare ESC, so no "[31m" residue reaches the screen.
func stripEsc(s string) string { return escSeq.ReplaceAllString(ansi.Strip(s), "") }

func paneText(s string) string { return textutil.OneLine(stripEsc(s)) }

// paneState is the pane between frames: rendered lines, the scroll, follow (pinned to the newest) and live.
type paneState struct {
	lines        []string
	top          int
	follow, live bool
	rows         int
	raw          bool
	noLog        bool   // the job has no raw log to toggle to
	open         bool   // the last line is a text line that the next text delta continues
	openText     string // the raw text of that line
}

// paneSet is both views of one job: the events and the raw log each keep their own scroll and follow state.
type paneSet struct {
	ev, raw paneState
	onRaw   bool
}

func newPaneSet() paneSet {
	return paneSet{ev: newPane(), raw: paneState{follow: true, live: true, raw: true}}
}

func (ps paneSet) cur() paneState {
	if ps.onRaw {
		return ps.raw
	}
	return ps.ev
}

// paneSetKey switches view on right (the `l` key, which the pane does not otherwise use), else scrolls the shown one.
func paneSetKey(ps paneSet, k key) paneSet {
	switch {
	case k == keyRight:
		ps.onRaw = !ps.onRaw
	case ps.onRaw:
		ps.raw = paneOnKey(ps.raw, k)
	default:
		ps.ev = paneOnKey(ps.ev, k)
	}
	return ps
}

// rawOnLine adds one log line, cleaned of escapes and controls and clipped; it never wraps.
func rawOnLine(s paneState, line []byte) paneState {
	text := textutil.TruncateCells(paneText(printable(string(line))), maxLineWidth, "…")
	return paneAppend(s, text)
}

// newPane is a pane that follows a job until its result arrives.
func newPane() paneState { return paneState{follow: true, live: true} }

func paneOnLine(s paneState, line []byte) paneState {
	var ev wire.Event
	if json.Unmarshal(line, &ev) != nil {
		return paneClamp(s)
	}
	if ev.Type == wire.EventText && ev.Via == "" {
		return paneOnText(s, ev)
	}
	text, ok := paneEventLine(ev, maxLineWidth)
	if !ok {
		return paneClamp(s)
	}
	s.open = false
	s = paneAppend(s, text)
	if ev.Type == wire.EventResult {
		s.live = false
	}
	return s
}

// paneOnText joins text deltas into lines; a newline or a new turn ends the open line.
func paneOnText(s paneState, ev wire.Event) paneState {
	if ev.Sep {
		s.open = false
	}
	for i, part := range strings.Split(ev.Text, "\n") {
		if i > 0 {
			s.open = false
		}
		switch {
		case s.open:
			if len(s.openText) < 4*maxLineWidth {
				s.openText += part
			}
			s.lines[len(s.lines)-1] = textutil.TruncateCells(paneText(s.openText), maxLineWidth, "…")
		case strings.TrimSpace(part) != "":
			s.open, s.openText = true, part
			s = paneAppend(s, textutil.TruncateCells(paneText(part), maxLineWidth, "…"))
		}
	}
	return paneClamp(s)
}

func paneAppend(s paneState, text string) paneState {
	drop := max(len(s.lines)+1-tailKeep, 0)
	s.lines = append(slices.Clone(s.lines[drop:]), text)
	s.top -= drop
	return paneClamp(s)
}

// paneOnKey scrolls; up leaves follow, down onto the last page resumes it.
func paneOnKey(s paneState, k key) paneState {
	s = paneClamp(s)
	switch k {
	case keyUp:
		s.follow = false
		s.top--
	case keyDown:
		s.top++
		s.follow = s.top >= max(len(s.lines)-s.rows, 0)
	}
	return paneClamp(s)
}

func paneResize(s paneState, rows int) paneState {
	s.rows = max(rows, 0)
	return paneClamp(s)
}

func paneClamp(s paneState) paneState {
	s.rows = max(s.rows, 0)
	bottom := max(len(s.lines)-s.rows, 0)
	if s.follow {
		s.top = bottom
	}
	s.top = min(max(s.top, 0), bottom)
	return s
}

// paneHead is the header wrapped to width (the task to three lines) and a rule, cut to fit above the footer.
func paneHead(head []string, width, height int) []string {
	var lines []string
	for _, h := range head {
		limit := 1
		if strings.HasPrefix(h, "task ") {
			limit = 3
		}
		lines = append(lines, wrapCells(printable(h), width, limit)...)
	}
	lines = append(lines, strings.Repeat("─", width))
	return lines[:min(len(lines), max(height-footerRows, 0))]
}

// paneRows is how many event lines fit under head in a width x height terminal; the view passes it to paneResize.
func paneRows(head []string, width, height int) int {
	width = min(max(width, 0), maxLineWidth)
	return max(height-len(paneHead(head, width, height))-footerRows, 0)
}

// paneLines is the frame: head, rule, the scrolled event lines, then the footer.
func paneLines(s paneState, head []string, width, height int) []string {
	if height <= 0 {
		return nil
	}
	width = min(max(width, 0), maxLineWidth)
	lines := paneHead(head, width, height)
	s = paneResize(s, max(height-len(lines)-footerRows, 0))
	for _, line := range s.lines[s.top:min(s.top+s.rows, len(s.lines))] {
		lines = append(lines, textutil.TruncateCells(line, width, "…"))
	}
	if len(lines)+1 < height {
		lines = append(lines, "")
	}
	state, hint := "settled", "l log"
	if s.live {
		state = "live"
	}
	if s.raw {
		state, hint = state+" · log", "l events"
	}
	foot := state + " · ↑/↓ scroll · " + hint + " · " + jobFooter
	if s.noLog {
		foot = state + " · ↑/↓ scroll · " + jobFooter
	}
	return append(lines, textutil.TruncateCells(foot, width, "…"))
}

// eventThinking is the pane's own event type for a transcript's thinking block; the wire has none.
const eventThinking = "thinking"

// transcriptPoll is how often a subagent's transcript is read again for new lines.
const transcriptPoll = 300 * time.Millisecond

// transcriptStream tails a subagent transcript as wire event lines until it finishes or ctx ends.
func transcriptStream(path string) streamFunc {
	return func(ctx context.Context, _ agent.Record, w io.Writer) error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		t := &transcriptTail{r: bufio.NewReaderSize(f, readBuf), enc: json.NewEncoder(w), names: map[string]string{}}
		for {
			settled := transcriptFinished(path) // read before the drain, so the last lines are never missed
			if err := t.drain(settled); err != nil {
				return err
			}
			if settled {
				return t.enc.Encode(wire.Event{Type: wire.EventResult, Status: "finished"})
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(transcriptPoll):
			}
		}
	}
}

// transcriptTail is one transcript read: the unfinished line, its truncation flag and the tool names seen.
type transcriptTail struct {
	r       *bufio.Reader
	enc     *json.Encoder
	names   map[string]string
	partial []byte
	cut     bool
}

// drain emits every complete line; with last set it also emits a final line that has no newline.
func (t *transcriptTail) drain(last bool) error {
	for {
		done, _ := readCapped(t.r, &t.partial, &t.cut) // an error is EOF: the rest of the line has not landed yet
		if !done && (!last || len(t.partial) == 0) {
			return nil
		}
		if err := t.emit(); err != nil {
			return err
		}
		if !done {
			return nil
		}
	}
}

func (t *transcriptTail) emit() error {
	evs := transcriptEvents(t.partial, t.names)
	if t.cut {
		evs = []wire.Event{{Type: wire.EventText, Text: "…(line truncated)\n"}}
	}
	t.partial, t.cut = t.partial[:0], false
	if cap(t.partial) > 2*readBuf {
		t.partial = nil
	}
	for _, ev := range evs {
		if err := t.enc.Encode(ev); err != nil {
			return err
		}
	}
	return nil
}

// readBuf is the transcript reader's buffer size.
const readBuf = 64 << 10

// readCapped adds r's next line to partial up to paneLineBytes (cut past that); true once the newline was read.
func readCapped(r *bufio.Reader, partial *[]byte, cut *bool) (bool, error) {
	for {
		chunk, err := r.ReadSlice('\n')
		if room := paneLineBytes - len(*partial); len(chunk) <= room {
			*partial = append(*partial, chunk...)
		} else {
			*partial, *cut = append(*partial, chunk[:room]...), true
		}
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		}
		return false, err
	}
}

func transcriptFinished(path string) bool {
	raw, err := os.ReadFile(metaPath(path))
	if err != nil {
		return false
	}
	m, _ := parseMeta(raw)
	return m.Finished || readJournal(filepath.Dir(path))[subagentIDFromPath(path)].Resulted
}

// transcriptLine is the record subset the pane summarises; content is a string or an array of blocks.
type transcriptLine struct {
	Type    string `json:"type"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type transcriptBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// transcriptEvents turns one transcript record into pane events; names maps tool_use ids to tool names for results.
func transcriptEvents(raw []byte, names map[string]string) []wire.Event {
	var rec transcriptLine
	if len(raw) > paneLineBytes || json.Unmarshal(raw, &rec) != nil || (rec.Type != "assistant" && rec.Type != "user") {
		return nil
	}
	var blocks []transcriptBlock
	if json.Unmarshal(rec.Message.Content, &blocks) != nil {
		return nil // a user turn's plain string is the brief, not progress
	}
	home, _ := os.UserHomeDir()
	var out []wire.Event
	for _, b := range blocks {
		if ev, ok := blockEvent(b, names, home); ok {
			out = append(out, ev)
		}
	}
	return out
}

func blockEvent(b transcriptBlock, names map[string]string, home string) (wire.Event, bool) {
	switch b.Type {
	case "text":
		return wire.Event{Type: wire.EventText, Text: firstLine(b.Text) + "\n"}, true
	case "thinking":
		line := firstLine(b.Thinking)
		return wire.Event{Type: eventThinking, Text: line}, line != ""
	case "tool_use":
		names[b.ID] = b.Name
		arg := cmp.Or(toolArg(b.Input), inputField(b.Input, "description"), inputField(b.Input, "query"))
		return wire.Event{Type: wire.EventToolUse, Tool: &wire.ToolRef{Name: b.Name, ID: b.ID},
			Text: stripPaths(arg, home, "")}, true
	case "tool_result":
		ev := wire.Event{Type: wire.EventToolResult, Tool: &wire.ToolRef{Name: names[b.ToolUseID], ID: b.ToolUseID},
			Text: firstLine(blockText(b.Content))}
		delete(names, b.ToolUseID)
		if b.IsError {
			ev.Status = "error"
		}
		return ev, true
	}
	return wire.Event{}, false
}

func inputField(input json.RawMessage, key string) string {
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// blockText is a tool result's text: its string content, or the text parts of a block array.
func blockText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct{ Text string }
	_ = json.Unmarshal(raw, &parts)
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text + "\n")
	}
	return b.String()
}

// firstLine is the first non-blank line of s, trimmed.
func firstLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// Oversized event lines are dropped whole so an unfinished tool dump cannot grow the reader without bound.
const paneLineBytes = 1 << 20

// paneFeed is the pane's one reader goroutine; the UI loop only drains its ring of complete lines.
type paneFeed struct {
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	mu      sync.Mutex
	pending [tailKeep][]byte
	next    int
	count   int
	partial []byte
	discard bool
}

// openPane starts reading rec's stream; it ends when the stream does, on close, or when ctx ends.
func openPane(ctx context.Context, rec agent.Record) *paneFeed { return openFeed(ctx, rec, paneStream) }

func openFeed(ctx context.Context, rec agent.Record, stream streamFunc) *paneFeed {
	ctx, cancel := context.WithCancel(ctx)
	p := &paneFeed{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		defer cancel()
		if err := stream(ctx, rec, p); err != nil && ctx.Err() == nil {
			_ = json.NewEncoder(p).Encode(wire.Event{Type: wire.EventError, Text: err.Error()})
		}
	}()
	return p
}

// Write splits the stream into lines; after close it fails, so the stream's copy loop returns.
func (p *paneFeed) Write(b []byte) (int, error) {
	n := len(b)
	for len(b) > 0 {
		if err := p.ctx.Err(); err != nil {
			return n - len(b), err
		}
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			p.push(b, false)
			break
		}
		p.push(b[:i], true)
		b = b[i+1:]
	}
	return n, nil
}

func (p *paneFeed) push(b []byte, complete bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.partial)+len(b) > paneLineBytes {
		p.partial, p.discard = nil, true
	}
	if !p.discard {
		p.partial = append(p.partial, b...)
	}
	if !complete {
		return
	}
	if !p.discard {
		p.pending[p.next] = p.partial
		p.next = (p.next + 1) % tailKeep
		p.count = min(p.count+1, tailKeep)
	}
	p.partial, p.discard = nil, false
}

// drain takes the complete lines read since the last drain, oldest first; it never waits on a file.
func (p *paneFeed) drain() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]byte, p.count)
	for i := range out {
		at := (p.next - p.count + i + tailKeep) % tailKeep
		out[i], p.pending[at] = p.pending[at], nil
	}
	p.count = 0
	return out
}

func (p *paneFeed) finished() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *paneFeed) close() {
	p.cancel()
	<-p.done
}

// paneFeeds are the pane's readers; the raw log's starts the first time that view opens and never restarts.
type paneFeeds struct {
	ev, raw *paneFeed
	rawFn   streamFunc // the raw view's stream; nil for a job with no log
}

// onKey applies a key to the views and starts the raw reader when the log view is first shown.
func (f *paneFeeds) onKey(ctx context.Context, rec agent.Record, ps paneSet, k key) paneSet {
	if k == keyRight && f.rawFn == nil {
		return ps
	}
	ps = paneSetKey(ps, k)
	if ps.onRaw && f.raw == nil {
		f.raw = openFeed(ctx, rec, f.rawFn)
	}
	return ps
}

func (f *paneFeeds) close() {
	f.ev.close()
	if f.raw != nil {
		f.raw.close()
	}
}

// paneDrain feeds each view the lines read since the last frame; the raw view settles once its log ended.
func paneDrain(ps paneSet, f *paneFeeds) paneSet {
	for _, l := range f.ev.drain() {
		ps.ev = paneOnLine(ps.ev, l)
	}
	if f.raw != nil {
		ended := f.raw.finished() // checked before the drain, so the last lines are never missed
		for _, l := range f.raw.drain() {
			ps.raw = rawOnLine(ps.raw, l)
		}
		ps.raw.live = ps.raw.live && !ended
	}
	return ps
}
