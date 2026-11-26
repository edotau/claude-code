package tree

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
)

func TestEventLine(t *testing.T) {
	line := func(raw string, width int) (string, bool) {
		var ev wire.Event
		if json.Unmarshal([]byte(raw), &ev) != nil {
			return "", false
		}
		return paneEventLine(ev, width)
	}
	tests := []struct {
		name, raw, want string
		ok              bool
	}{
		{"text", `{"schema":1,"seq":4,"t_ms":812,"type":"text","text":"hello\nworld"}`, "hello world", true},
		{"via", `{"schema":1,"dispatch_id":"20261123T055007.452-opencode","via":"20261123T055008.173-opencode",` +
			`"agent":"opencode","seq":7,"t_ms":2210,"type":"tool_use","tool":{"name":"read","id":"call_1"}}`,
			"↳ opencode·055008 ▸ read", true},
		{"pi tool_use", `{"schema":1,"type":"tool_use","tool":{"name":"bash"},"text":"ls -la"}`, "▸ bash ls -la", true},
		{"pi tool_result", `{"schema":1,"type":"tool_result","tool":{"name":"bash"},"status":"error","text":"boom"}`,
			"✗ bash error boom", true},
		{"controls", `{"schema":1,"seq":9,"type":"tool_result","text":"\u001b[31mred\u001b[0m\u0007\u009b\tend"}`,
			"✓ red end", true},
		{"heartbeat", `{"schema":1,"dispatch_id":"x","type":"heartbeat","t_ms":15000,"live":"quiet"}`, "", false},
	}
	for _, tt := range tests {
		got, ok := line(tt.raw, 80)
		if got != tt.want || ok != tt.ok || strings.ContainsFunc(got, unicode.IsControl) {
			t.Errorf("%s: paneEventLine = %q, %v; want %q, %v", tt.name, got, ok, tt.want, tt.ok)
		}
	}
	long := `{"schema":1,"type":"text","text":"` + strings.Repeat("x", 100) + `"}`
	if got, _ := line(long, 200); len([]rune(got)) != maxLineWidth || !strings.HasSuffix(got, "…") {
		t.Errorf("wide terminal line = %q, want %d cells ending in …", got, maxLineWidth)
	}
}

func TestPaneScrollClamp(t *testing.T) {
	s := paneResize(newPane(), 2)
	for i := range 4 {
		s = paneOnLine(s, fmt.Appendf(nil, `{"schema":1,"type":"text","text":"line %d\n"}`, i))
	}
	if s.top != 2 {
		t.Fatalf("following top = %d, want 2", s.top)
	}
	for range 10 {
		s = paneOnKey(s, keyUp)
	}
	if s.top != 0 || s.follow {
		t.Fatalf("after up: %+v, want top 0 and not following", s)
	}
	if s = paneOnLine(s, []byte(`{"schema":1,"type":"text","text":"line 4\n"}`)); s.top != 0 {
		t.Fatalf("a new line moved an unfollowed pane: top %d", s.top)
	}
	for range 10 {
		s = paneOnKey(s, keyDown)
	}
	if !s.follow || s.top != 3 {
		t.Fatalf("after down: %+v, want following at top 3", s)
	}
}

func TestPaneLinesFitTerminal(t *testing.T) {
	head := []string{"agent  opencode", "task   " + strings.Repeat("read every file ", 20)}
	s := newPane()
	for i := range 40 {
		s = paneOnLine(s, fmt.Appendf(nil, `{"schema":1,"type":"text","text":"line %d\n"}`, i))
	}
	for _, size := range [][2]int{{100, 30}, {40, 8}} {
		w, h := size[0], size[1]
		lines := paneLines(paneResize(s, paneRows(head, w, h)), head, w, h)
		if len(lines) > h || !strings.Contains(strings.Join(lines, "\n"), "line 39") {
			t.Errorf("%dx%d: %d lines, newest missing: %q", w, h, len(lines), lines)
		}
		for _, l := range lines {
			if n := len([]rune(l)); n > min(w, maxLineWidth) {
				t.Errorf("%dx%d: line of %d cells: %q", w, h, n, l)
			}
		}
	}
}

func TestPaneSettlesOnResult(t *testing.T) {
	orig := paneStream
	t.Cleanup(func() { paneStream = orig })
	paneStream = func(_ context.Context, _ agent.Record, w io.Writer) error {
		_, err := io.WriteString(w, `{"schema":1,"type":"text","text":"done"}`+"\n"+
			`{"schema":1,"dispatch_id":"20261123T055007.452-opencode","type":"result","status":"ok"}`+"\n")
		return err
	}
	p := openPane(t.Context(), agent.Record{})
	t.Cleanup(p.close)
	select {
	case <-p.done:
	case <-time.After(time.Second):
		t.Fatal("reader did not end with the stream")
	}
	s := newPane()
	for _, line := range p.drain() {
		s = paneOnLine(s, line)
	}
	if s.live || len(s.lines) != 2 || s.lines[1] != "result ok" {
		t.Fatalf("pane after result = %+v, want settled with 2 lines", s)
	}
}

func TestPaneToggleKeepsEachViewsScroll(t *testing.T) {
	ps := newPaneSet()
	ps.ev, ps.raw = paneResize(ps.ev, 2), paneResize(ps.raw, 2)
	for i := range 6 {
		ps.ev = paneOnLine(ps.ev, fmt.Appendf(nil, `{"schema":1,"type":"text","text":"e%d\n"}`, i))
		ps.raw = rawOnLine(ps.raw, fmt.Appendf(nil, "raw %d", i))
	}
	ps = paneSetKey(paneSetKey(ps, keyUp), keyUp) // events scrolled up and off follow
	ps = paneSetKey(ps, keyRight)
	if !ps.onRaw || !ps.raw.follow || ps.raw.top != 4 || ps.ev.follow || ps.ev.top != 2 {
		t.Fatalf("after l: %+v, want raw view following at 4 and events parked at 2", ps)
	}
	if ps = paneSetKey(ps, keyRight); ps.onRaw || ps.ev.top != 2 || ps.ev.follow {
		t.Fatalf("back on events: %+v, want the old scroll kept", ps)
	}
}

func TestRawLineIsCleanAndClipped(t *testing.T) {
	line := "\x1b[31mred\x1b[0m\x07\tgo \r\x1b]0;t\x07" + strings.Repeat("x", 300)
	s := rawOnLine(paneResize(paneState{follow: true, raw: true}, 3), []byte(line))
	lines := paneLines(s, nil, 40, 6)
	for _, l := range lines {
		if strings.ContainsFunc(l, unicode.IsControl) || strings.Contains(l, "[31m") || len([]rune(l)) > 40 {
			t.Errorf("raw frame line not clean or over 40 cells: %q", l)
		}
	}
	if len(lines) > 6 || !strings.Contains(lines[len(lines)-1], "l events") {
		t.Errorf("frame = %q, want at most 6 lines ending in the events hint", lines)
	}
}
