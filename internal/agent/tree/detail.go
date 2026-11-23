// detail.go is the dashboard's detail pane: one job's event stream, one clipped line per event, followed while live.

package tree

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/ansi"
)

// streamFunc writes a job's event lines to w until the job settles or ctx ends.
type streamFunc func(ctx context.Context, rec agent.Record, w io.Writer) error

// paneStream is what `agents result -f --events` streams; a carrier's child events need ask's streamEvents exported.
var paneStream streamFunc = func(ctx context.Context, rec agent.Record, w io.Writer) error {
	_, err := agent.TailEvents(ctx, rec, true, 0, w)
	return err
}

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
	if ev.Type != wire.EventText {
		parts = append(parts, ev.Type)
	}
	if ev.Tool != nil && ev.Tool.Name != "" {
		parts = append(parts, ev.Tool.Name)
	}
	if ev.Status != "" {
		parts = append(parts, ev.Status)
	}
	if ev.Text != "" {
		parts = append(parts, ev.Text)
	}
	return textutil.TruncateCells(paneText(strings.Join(parts, " ")), min(max(width, 0), maxLineWidth), "…"), true
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

// paneText removes escape sequences before OneLine drops the bare ESC, so no "[31m" residue reaches the screen.
func paneText(s string) string {
	return textutil.OneLine(escSeq.ReplaceAllString(ansi.Strip(s), ""))
}

// paneState is the pane between frames: rendered lines, the scroll, follow (pinned to the newest) and live.
type paneState struct {
	lines        []string
	top          int
	follow, live bool
	rows         int
}

// newPane is a pane that follows a job until its result arrives.
func newPane() paneState { return paneState{follow: true, live: true} }

func paneOnLine(s paneState, line []byte) paneState {
	var ev wire.Event
	if json.Unmarshal(line, &ev) != nil {
		return paneClamp(s)
	}
	text, ok := paneEventLine(ev, maxLineWidth)
	if !ok {
		return paneClamp(s)
	}
	drop := max(len(s.lines)+1-tailKeep, 0)
	s.lines = append(slices.Clone(s.lines[drop:]), text)
	s.top -= drop
	s = paneClamp(s)
	if ev.Type == wire.EventResult {
		s.live = false
	}
	return s
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
		lines = append(lines, wrapCells(printable(escSeq.ReplaceAllString(h, "")), width, limit)...) // keeps alignment
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
	foot := "settled · ↑/↓ scroll · " + jobFooter
	if s.live {
		foot = "live · ↑/↓ scroll · " + jobFooter
	}
	return append(lines, textutil.TruncateCells(foot, width, "…"))
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
func openPane(ctx context.Context, rec agent.Record) *paneFeed {
	ctx, cancel := context.WithCancel(ctx)
	p := &paneFeed{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	stream := paneStream
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

func (p *paneFeed) close() {
	p.cancel()
	<-p.done
}
