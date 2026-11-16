package agent

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// `agents diff --verify <id>`: routing proof for one dispatch, from its record and the proxy log.

const (
	verifyChunk     = 256 << 10
	verifyReadCap   = 8 << 20
	verifyTailBytes = 8 << 10
	windowSlack     = 2 * time.Second
)

// verifyScanCap bounds how many bytes of the proxy log a verify reads from its end; a var so a test can shrink it.
var verifyScanCap int64 = 16 << 20

func proxyLogPath() string { return filepath.Join(paths.StateDir(), "proxy.log") }

type VerifyTokens struct {
	In  int64 `json:"in"`
	Out int64 `json:"out"`
}

// ProxyMatch counts the proxy-log requests attributed to one dispatch; Match says how exact that attribution is.
type ProxyMatch struct {
	Match    string         `json:"match"`
	Requests int            `json:"requests"`
	Remote   int            `json:"remote"`
	Hosts    []string       `json:"hosts"`
	Broken   int            `json:"stream_broken"`
	NonOK    map[string]int `json:"non_200"`
	Note     string         `json:"note"`
}

// VerifyReport is the routing proof for one dispatch (`agents diff --verify <id>`).
type VerifyReport struct {
	ID         string        `json:"id"`
	Agent      string        `json:"agent"`
	Workspace  string        `json:"workspace"`
	Route      string        `json:"route,omitempty"`
	Model      string        `json:"model"`
	Status     string        `json:"status"`
	Fallback   string        `json:"fallback"`
	Start      string        `json:"start"`
	End        string        `json:"end"`
	DurationMs int64         `json:"duration_ms"`
	Tokens     *VerifyTokens `json:"tokens,omitempty"`
	Proxy      ProxyMatch    `json:"proxy"`
	Mismatch   bool          `json:"mismatch"`
	Mismatches []string      `json:"mismatches,omitempty"`
	Wrote      *[]string     `json:"wrote,omitempty"` // Record.Wrote; nil: not observed

	owned []string // the Owned: paths of the dispatch's brief; unset for a brief without that line
}

// verifyDispatch builds the routing proof for dispatch id from its record and the proxy log at logPath.
func verifyDispatch(id, logPath string) (*VerifyReport, error) {
	rec, err := LoadRecord(id)
	if err != nil {
		return nil, fmt.Errorf("--verify %q: %w", id, err)
	}
	start := rec.Time.Truncate(time.Second)
	end := time.Now()
	if rec.DurationMs > 0 {
		end = rec.Time.Add(time.Duration(rec.DurationMs) * time.Millisecond)
	}
	rep := &VerifyReport{ID: rec.ID, Agent: rec.Agent, Workspace: rec.Provider, Route: rec.Route, Model: rec.Model,
		Status: rec.Status, Fallback: fallbackText(rec), DurationMs: rec.DurationMs, Tokens: logTokens(rec.LogPath()),
		Start: start.Local().Format(time.RFC3339), End: end.Local().Format(time.RFC3339),
		Mismatches: routingMismatches(rec), Wrote: rec.Wrote, owned: ParseOwned(rec.Request.Prompt)}
	rep.Mismatch = len(rep.Mismatches) > 0
	rep.Proxy = matchProxy(logPath, rec, start, end.Add(windowSlack))
	return rep, nil
}

func fallbackText(rec Record) string {
	if rec.Fallback == "" && rec.RanAs == "" {
		return "none"
	}
	s := rec.Fallback + " -> " + cmp.Or(rec.RanAs, rec.Agent)
	if rec.FallbackReason != "" {
		s += " (" + rec.FallbackReason + ")"
	}
	return s
}

// routingMismatches lists where the run differs from the request: a fallback, another workspace, another model.
func routingMismatches(rec Record) []string {
	var out []string
	if rec.Fallback != "" || rec.RanAs != "" {
		out = append(out, "fallback: "+fallbackText(rec))
	}
	if p := rec.Request.Provider; p != "" && !strings.Contains(rec.Provider+" "+rec.Route, p) {
		out = append(out, fmt.Sprintf("workspace: asked for %q, ran on %q", p, cmp.Or(rec.Route, rec.Provider)))
	}
	return append(out, modelMismatch(rec)...)
}

// modelMismatch compares the requested model with the one that ran; an alias that is part of the real name passes.
func modelMismatch(rec Record) []string {
	m := rec.Request.Model
	if m == "" || rec.Model == "" || strings.Contains(rec.Model+" "+rec.Invoked, m) {
		return nil
	}
	return []string{fmt.Sprintf("model: asked for %q, ran %q", m, rec.Model)}
}

var tokensRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`tokens in=(\d+) out=(\d+)`) })

// logTokens reads the last "tokens in=N out=M" line from the tail of the dispatch's job log.
func logTokens(logPath string) *VerifyTokens {
	f, err := os.Open(logPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	off := max(0, st.Size()-verifyTailBytes)
	buf, _ := io.ReadAll(io.NewSectionReader(f, off, st.Size()-off))
	all := tokensRe().FindAllSubmatch(buf, -1)
	if len(all) == 0 {
		return nil
	}
	last := all[len(all)-1]
	in, _ := strconv.ParseInt(string(last[1]), 10, 64)
	out, _ := strconv.ParseInt(string(last[2]), 10, 64)
	return &VerifyTokens{In: in, Out: out}
}

type proxyLine struct {
	ts                                         time.Time
	profile, model, status, via, req, dispatch string
	broken                                     bool
}

// parseProxyLine reads a proxy log line: fixed fields, then via=, dispatch=, req=, stream=broken[:class] tokens.
func parseProxyLine(line string) (proxyLine, bool) {
	f := strings.Fields(line)
	if len(f) < 4 {
		return proxyLine{}, false
	}
	ts, err := time.Parse(time.RFC3339, f[0])
	if err != nil {
		return proxyLine{}, false
	}
	pl := proxyLine{ts: ts, profile: f[1], model: f[2], status: f[3]}
	for _, kv := range f[4:] {
		if strings.HasPrefix(kv, "msg=") { // the upstream error text is quoted and last: never read metadata from it
			break
		}
		pl.setToken(kv)
	}
	return pl, true
}

func (pl *proxyLine) setToken(kv string) {
	switch {
	case strings.HasPrefix(kv, "via="):
		pl.via = kv[4:]
	case strings.HasPrefix(kv, "dispatch="):
		pl.dispatch = kv[9:]
	case strings.HasPrefix(kv, "req="):
		pl.req = kv[4:]
	case kv == "stream=broken" || strings.HasPrefix(kv, "stream=broken:"):
		pl.broken = true
	}
}

// scanBackward calls visit on each line of r from the last to the first until visit returns false.
func scanBackward(r io.ReaderAt, size int64, visit func(line string) bool) error {
	var carry []byte
	for end := size; end > 0; {
		start := max(0, end-verifyChunk)
		buf := make([]byte, end-start, end-start+int64(len(carry)))
		if _, err := r.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		lines := bytes.Split(append(buf, carry...), []byte{'\n'})
		first := 0
		if start > 0 {
			carry, first = lines[0], 1
		}
		for i := len(lines) - 1; i >= first; i-- {
			if len(lines[i]) > 0 && !visit(string(lines[i])) {
				return nil
			}
		}
		end = start
	}
	return nil
}

// matchProxy counts the proxy lines of rec's profile and model inside [start, end]; request ids make it exact.
func matchProxy(logPath string, rec Record, start, end time.Time) ProxyMatch {
	pm := ProxyMatch{Match: "none", Hosts: []string{}, NonOK: map[string]int{}}
	if rec.Provider == "" || rec.Model == "" {
		pm.Note = "the record names no provider or model, so it has no proxy profile to match"
		return pm
	}
	tagged, hits, capped, err := windowLines(logPath, rec, start, end)
	if err != nil {
		pm.Note = "proxy log unreadable: " + err.Error()
		return pm
	}
	capNote := ""
	if capped {
		capNote = fmt.Sprintf("; scan stopped after %d bytes of the proxy log: older lines not read", verifyScanCap)
	}
	if len(tagged) > 0 {
		pm.Match, pm.Note = "dispatch id", "matched by dispatch= tags on the proxy log lines: exact"+capNote
		tally(&pm, tagged)
		return pm
	}
	pm.Match = "time window"
	pm.Note = fmt.Sprintf("no proxy log line carries dispatch=%s; fell back to the time window over untagged lines "+
		"(%s %s, %s..%s): counts may include other traffic on the same workspace and model", rec.ID, rec.Provider,
		rec.Model, start.Local().Format("15:04:05"), end.Local().Format("15:04:05"))
	if exact := exactLines(rec, hits); len(exact) > 0 {
		hits, pm.Match, pm.Note = exact, "request id", "matched by req= ids that also appear in the dispatch's own log: exact"
	}
	tally(&pm, hits)
	pm.Note += capNote
	return pm
}

// proxyKind: "tagged" for dispatch=<rec.ID>, "window" for an untagged in-range line in rec's workspace, else "".
func proxyKind(pl proxyLine, rec Record, start, end time.Time) string {
	switch {
	case pl.dispatch == rec.ID:
		return "tagged"
	case pl.dispatch == "" && !pl.ts.Before(start) && !pl.ts.After(end) && pl.profile == rec.Provider && pl.model == rec.Model:
		return "window"
	}
	return ""
}

// windowLines reads the proxy log from its end up to verifyScanCap bytes (capped says it was hit), never by timestamp.
func windowLines(logPath string, rec Record, start, end time.Time) (tagged, window []proxyLine, capped bool, err error) {
	f, err := os.Open(logPath)
	if err != nil {
		return nil, nil, false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, nil, false, err
	}
	var read int64
	err = scanBackward(f, st.Size(), func(line string) bool {
		if read += int64(len(line)) + 1; read > verifyScanCap {
			capped = true
			return false
		}
		if pl, ok := parseProxyLine(line); ok {
			switch proxyKind(pl, rec, start, end) {
			case "tagged":
				tagged = append(tagged, pl)
			case "window":
				window = append(window, pl)
			}
		}
		return true
	})
	return tagged, window, capped, err
}

// exactLines keeps the lines whose req= id appears in the dispatch's job log or event file.
func exactLines(rec Record, hits []proxyLine) []proxyLine {
	if !slices.ContainsFunc(hits, func(l proxyLine) bool { return l.req != "" }) {
		return nil
	}
	var text strings.Builder
	for _, p := range []string{rec.LogPath(), rec.EventsPath()} {
		if f, err := os.Open(p); err == nil {
			_, _ = io.Copy(&text, io.LimitReader(f, verifyReadCap))
			_ = f.Close()
		}
	}
	return slices.DeleteFunc(slices.Clone(hits), func(l proxyLine) bool {
		return l.req == "" || !strings.Contains(text.String(), l.req)
	})
}

func tally(pm *ProxyMatch, hits []proxyLine) {
	hosts := map[string]bool{}
	for _, h := range hits {
		pm.Requests++
		if h.via != "" {
			pm.Remote++
			hosts[h.via] = true
		}
		if h.broken {
			pm.Broken++
		}
		if h.status != "200" {
			pm.NonOK[h.status]++
		}
	}
	pm.Hosts = slices.Sorted(maps.Keys(hosts))
}
