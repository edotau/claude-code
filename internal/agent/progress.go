// progress.go labels a running dispatch healthy, quiet or hung from the files its run already writes.
package agent

import (
	"bytes"
	"cmp"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/textutil"
)

const (
	progressWidth   = 80                              // the line's column budget everywhere it shows
	doingWidth      = 20                              // columns the "doing" field may take before the clip
	noSignalPast    = 10*time.Minute + 15*time.Second // a no-stream agent's ⚠ threshold
	progressMaxRead = 8 << 20                         // bound of one events or log read
	toolEventMark   = `"type":"tool_use"`
)

// Progress is what the harness can tell about a running dispatch right now; Tools < 0 means unknown.
type Progress struct {
	Agent, Model, Doing string
	Elapsed, Quiet      time.Duration
	Tools               int
	Stream              bool // the agent has a quiet/hung threshold; false: no live signal to judge by
	Seen                bool // a no-stream agent showed any sign of life
	quietAt, hungAt     time.Duration
}

// progressLimits are the quiet and hung thresholds per agent, derived from the harness's own idle limits.
func progressLimits(agent string) (quiet, hung time.Duration, ok bool) {
	if agent == "claude" {
		return 45 * time.Second, 90 * time.Second, true // internal/claude/idle.go cuts a silent run at 90 s
	}
	if lim := IdleLimitFor(agent); lim > 0 {
		return lim / 2, lim, true
	}
	return 0, 0, false
}

// ProgressOf reads r's own dispatch files (never the directory) at now; a missing file drops its field.
func ProgressOf(r Record, now time.Time) Progress {
	agent := cmp.Or(r.RanAs, r.Agent)
	p := Progress{Agent: agent, Model: cmp.Or(r.Model, r.Invoked, r.Request.Model, r.Route),
		Elapsed: max(now.Sub(r.Time), 0), Tools: -1}
	p.quietAt, p.hungAt, p.Stream = progressLimits(agent)
	last, _ := LastSignal(r)
	p.Quiet = max(now.Sub(last), 0)
	if b, ok := readCapped(r.EventsPath()); ok {
		p.Tools = bytes.Count(b, []byte(toolEventMark))
	}
	if tail, err := tailBytes(r.LogPath()); err == nil {
		p.Doing = doingOf(string(tail))
	}
	if p.Tools <= 0 && agent == "gemini" { // gemini's events carry no tool_use: its log's finished steps are the count
		if b, ok := readCapped(r.LogPath()); ok {
			p.Tools = max(p.Tools, countDoneSteps(b))
		}
	}
	p.Seen = p.Doing != "" || p.Tools > 0
	return p
}

// readCapped reads the first progressMaxRead bytes of path; ok is false when it cannot be read.
func readCapped(path string) ([]byte, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, progressMaxRead))
	return b, err == nil
}

// countDoneSteps counts the log's finished `▸ step: <type> done` lines (gemini).
func countDoneSteps(b []byte) int {
	n := 0
	for l := range bytes.SplitSeq(b, []byte("\n")) {
		if bytes.HasPrefix(l, []byte("▸ step: ")) && bytes.HasSuffix(l, []byte(" done")) {
			n++
		}
	}
	return n
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
	case p.Quiet >= p.quietAt:
		return "quiet " + progressDur(p.Quiet), false
	}
	return "last event " + progressDur(p.Quiet) + " ago", false
}

// Line is the one-line label, at most width columns; the doing field is clipped first, unknown fields dropped.
func (p Progress) Line(width int) string {
	sig, hung := p.signal()
	noLive := strings.HasPrefix(sig, "⚠ no live")
	parts := []string{p.Agent, p.Model, progressDur(p.Elapsed), sig}
	if p.Tools >= 0 && !noLive {
		parts = append(parts, fmt.Sprintf("%d tools", p.Tools))
	}
	head := strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), " · ")
	label := ""
	if hung {
		label = "last: "
	}
	room := min(doingWidth, width-textutil.DisplayWidth(head)-3-len(label))
	if p.Doing != "" && !noLive && room >= 4 {
		head += " · " + label + textutil.TruncateCells(p.Doing, room, "…")
	}
	return textutil.TruncateCells(head, width, "…")
}
