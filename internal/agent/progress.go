// progress.go labels a running dispatch healthy, quiet or hung from the files its run already writes.
package agent

import (
	"bufio"
	"bytes"
	"cmp"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/textutil"
)

const (
	progressWidth = 80                              // the line's column budget everywhere it shows
	doingWidth    = 20                              // columns the "doing" field may take before the clip
	noSignalPast  = 10*time.Minute + 15*time.Second // a no-stream agent's ⚠ threshold
	toolEventMark = `"type":"tool_use"`
)

// Progress is what the harness can tell about a running dispatch right now; Tools < 0 means unknown.
type Progress struct {
	Agent, Model, Doing string
	Label               string // the type label RowLabel gives the run; Agent stays the engine the limits key on
	Elapsed, Quiet      time.Duration
	Tools               int
	In, Out             int64 // live tokens so far (input incl. cache, output); 0 0: none reported
	ToolMs              int64 // the last finished tool's duration; 0: unknown
	Stream              bool  // the agent has a quiet/hung threshold; false: no live signal to judge by
	Seen                bool  // a no-stream agent showed any sign of life
	Phase               phase // what the log's last mark says the agent is busy with
	RetryIn             time.Duration
	quietAt, hungAt     time.Duration
}

// phase is a typed busy-state the stream log's last line can mark.
type phase int

const (
	phaseNone phase = iota
	phaseCompacting
	phaseRetrying
)

var retryInRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`retry_in=(\d+)ms$`) })

// phaseOf reads the last non-heartbeat log line: a `phase: compacting` or `retry:` mark, else none.
func phaseOf(tail string) (phase, time.Duration) {
	lines := strings.Split(strings.TrimRight(tail, "\n"), "\n")
	for _, line := range slices.Backward(lines) {
		l := strings.TrimSpace(line)
		switch {
		case !strings.HasPrefix(l, "▸ ") || strings.HasPrefix(l, "▸ heartbeat:"):
			continue
		case strings.HasPrefix(l, "▸ phase: compacting"):
			return phaseCompacting, 0
		case strings.HasPrefix(l, "▸ retry: "):
			m := retryInRe().FindStringSubmatch(l)
			if m == nil {
				return phaseNone, 0
			}
			ms, _ := strconv.ParseInt(m[1], 10, 64)
			return phaseRetrying, time.Duration(ms) * time.Millisecond
		}
		return phaseNone, 0
	}
	return phaseNone, 0
}

// progressLimits are the quiet and hung thresholds per agent, derived from the harness's own idle limits.
func progressLimits(agent string) (quiet, hung time.Duration, ok bool) {
	if lim := IdleLimitFor(agent); lim > 0 {
		return lim / 2, lim, true
	}
	return 0, 0, false
}

// ProgressOf reads r's own dispatch files (never the directory) at now; a missing file drops its field.
func ProgressOf(r Record, now time.Time) Progress {
	agent := cmp.Or(r.RanAs, r.Agent)
	p := Progress{Agent: agent, Label: RowLabel(r), Model: cmp.Or(r.Model, r.Invoked, r.Request.Model, route.Join(r.Route.Provider, r.Route.Profile)),
		Elapsed: max(now.Sub(r.Time), 0), Tools: -1}
	p.quietAt, p.hungAt, p.Stream = progressLimits(agent)
	last, _ := LastSignal(r)
	p.Quiet = max(now.Sub(last), 0)
	if n, ok := countFile(r.EventsPath(), countToolEvents); ok {
		p.Tools = n
	}
	if tail, err := tailBytes(r.LogPath()); err == nil {
		p.Doing = doingOf(string(tail))
		p.Phase, p.RetryIn = phaseOf(string(tail))
	}
	if p.Tools <= 0 && agent == "gemini" { // gemini's events carry no tool_use: its log's finished steps are the count
		if n, ok := countFile(r.LogPath(), countDoneSteps); ok {
			p.Tools = max(p.Tools, n)
		}
	}
	if a := r.Activity; a != nil {
		u := a.Usage
		p.In, p.Out, p.ToolMs = int64(u.InputTokens+u.CacheCreationTokens+u.CacheReadTokens), int64(u.OutputTokens), a.ToolMs
	}
	p.Seen = p.Doing != "" || p.Tools > 0 || p.In+p.Out > 0
	return p
}

// countFile streams path through count; ok is false when it cannot be read whole.
func countFile(path string, count func(io.Reader) (int, error)) (int, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	n, err := count(f)
	return n, err == nil
}

// progressChunk is the read size of the streaming counters.
var progressChunk = 64 << 10

// countToolEvents counts toolEventMark in r chunk by chunk, carrying a mark-sized overlap across reads.
func countToolEvents(r io.Reader) (int, error) {
	mark := []byte(toolEventMark)
	buf := make([]byte, progressChunk+len(mark)-1)
	n, keep := 0, 0
	for {
		got, err := r.Read(buf[keep:])
		data := buf[:keep+got]
		n += bytes.Count(data, mark)
		keep = copy(buf, data[max(len(data)-(len(mark)-1), 0):])
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
}

// countDoneSteps counts the log's finished `▸ step: <type> done` lines (gemini); long lines never abort it.
func countDoneSteps(r io.Reader) (int, error) {
	const suffix = " done"
	br := bufio.NewReaderSize(r, progressChunk)
	n := 0
	for {
		frag, more, err := br.ReadLine()
		match := bytes.HasPrefix(frag, []byte("▸ step: "))
		tail := append([]byte(nil), frag[max(len(frag)-len(suffix), 0):]...)
		for err == nil && more { // the suffix may straddle fragments: keep the last bytes seen
			frag, more, err = br.ReadLine()
			tail = append(tail, frag...)
			tail = tail[max(len(tail)-len(suffix), 0):]
		}
		if err == nil && match && bytes.HasSuffix(tail, []byte(suffix)) {
			n++
		}
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
}

var shellWrapRe = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(?:\S*/)?(?:ba|z)?sh -l?c (?:'(.*)'|"(.*)")$`)
})

// doingOf is the last non-heartbeat `▸ kind: text` line of tail, minus its prefix and any shell wrapper.
func doingOf(tail string) string {
	lines := strings.Split(strings.TrimRight(tail, "\n"), "\n")
	for _, line := range slices.Backward(lines) {
		l := strings.TrimSpace(line)
		if !strings.HasPrefix(l, "▸ ") || strings.HasPrefix(l, "▸ heartbeat:") {
			continue
		}
		_, text, ok := strings.Cut(strings.TrimPrefix(l, "▸ "), ": ")
		if !ok {
			return ""
		}
		if m := shellWrapRe().FindStringSubmatch(text); m != nil {
			text = cmp.Or(m[1], m[2])
		}
		text = strings.Join(strings.Fields(text), " ")
		if textutil.CredentialRE().MatchString(text) {
			return ""
		}
		return text
	}
	return ""
}

// progressDur spells d as 8s, 2m31s or 1h02m.
func progressDur(d time.Duration) string {
	s := int(max(d, 0).Round(time.Second).Seconds())
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case time.Duration(s)*time.Second < time.Hour:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh%02dm", s/3600, s%3600/60)
}

// toolsText is "21 tools"; empty when the count is unknown.
func (p Progress) toolsText() string {
	if p.Tools < 0 {
		return ""
	}
	return fmt.Sprintf("%d tools", p.Tools)
}

// usageText is "↑12.4k ↓350" (input incl. cache, output); empty when no usage is known.
func (p Progress) usageText() string {
	if p.In+p.Out == 0 {
		return ""
	}
	return "↑" + textutil.CompactCount(p.In) + " ↓" + textutil.CompactCount(p.Out)
}

// fitsBeside reports whether the doing field still shows in full (up to its cap) in avail columns.
func (p Progress) fitsBeside(avail int) bool {
	return p.Doing == "" || avail >= min(doingWidth, textutil.DisplayWidth(p.Doing))
}

// toolDur spells the last tool's duration as "tool 850ms" or "tool 12s"; empty when unknown.
func (p Progress) toolDur() string {
	switch {
	case p.ToolMs <= 0:
		return ""
	case p.ToolMs < 1000:
		return fmt.Sprintf("tool %dms", p.ToolMs)
	}
	return "tool " + progressDur(time.Duration(p.ToolMs)*time.Millisecond)
}

// inPhase is true while a compaction runs, or a retry's announced wait has not yet passed in silence.
func (p Progress) inPhase() bool {
	return p.Phase == phaseCompacting || p.Phase == phaseRetrying && p.Quiet <= p.RetryIn
}

// phaseText is "compacting 12s" (time quiet) or "retrying 4s" (the exact wait Pi announced).
func (p Progress) phaseText() string {
	if p.Phase == phaseRetrying {
		return "retrying " + progressDur(p.RetryIn)
	}
	return "compacting " + progressDur(p.Quiet)
}

// signal is the liveness field and whether the doing field is labelled "last:" (the run looks hung).
func (p Progress) signal() (text string, hung bool) {
	switch {
	case !p.Stream && !p.Seen && p.Elapsed > noSignalPast:
		return "⚠ no live signal past " + progressDur(noSignalPast), true
	case !p.Stream && !p.Seen:
		return "no live signal from " + p.Agent, false
	case !p.Stream:
		return "last event " + progressDur(p.Quiet) + " ago", false
	case p.Quiet >= p.hungAt:
		return "⚠ no event " + progressDur(p.Quiet), true
	case p.inPhase():
		return p.phaseText(), false
	case p.Quiet >= p.quietAt:
		return "quiet " + progressDur(p.Quiet), false
	}
	return "last event " + progressDur(p.Quiet) + " ago", false
}

// Line is the one-line label, at most width columns; the doing field is clipped first, unknown fields dropped.
func (p Progress) Line(width int) string {
	return p.render(width, cmp.Or(p.Label, p.Agent), p.Model, progressDur(p.Elapsed))
}

// Fragment is Line without agent, model and elapsed, for a row that already shows them.
func (p Progress) Fragment(width int) string { return p.render(width) }

// head joins lead, signal, tools and usage; on overflow a healthy "last event" and then the tool count yield.
func (p Progress) head(width int, sig string, noLive bool, lead []string) string {
	usage := p.usageText()
	join := func(sig, tools, usage string) string {
		parts := append(slices.Clone(lead), sig)
		if !noLive {
			parts = append(parts, tools, usage)
		}
		return strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), " · ")
	}
	head := join(sig, p.toolsText(), usage)
	if usage == "" || noLive || textutil.DisplayWidth(head) <= width {
		return head
	}
	if strings.HasPrefix(sig, "last event") {
		sig = ""
	}
	if head = join(sig, p.toolsText(), usage); textutil.DisplayWidth(head) <= width {
		return head
	}
	if head = join(sig, "", usage); textutil.DisplayWidth(head) <= width {
		return head
	}
	return textutil.TruncateCells(join(sig, "", ""), width-textutil.DisplayWidth(usage)-3, "…") + " · " + usage
}

func (p Progress) render(width int, lead ...string) string {
	sig, hung := p.signal()
	noLive := strings.HasPrefix(sig, "⚠ no live")
	head := p.head(width, sig, noLive, lead)
	label := ""
	if hung {
		label = "last: "
	}
	avail := width - textutil.DisplayWidth(head) - 3 - len(label)
	room := min(doingWidth, avail)
	if tm := p.toolDur(); tm != "" && !noLive && avail >= len(tm)+3 && p.fitsBeside(avail-len(tm)-3) {
		head += " · " + tm
		room = min(doingWidth, avail-len(tm)-3)
	}
	if p.Doing != "" && !noLive && room >= 4 {
		head += " · " + label + textutil.TruncateCells(p.Doing, room, "…")
	}
	return textutil.TruncateCells(head, width, "…")
}
