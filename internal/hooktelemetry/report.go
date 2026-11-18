package hooktelemetry

import (
	"bytes"
	"cmp"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

type View int

const (
	Activity View = 7
	Signals  View = 14
	budget        = 128 << 20
	lineCap       = 16 << 10
)

type signalRow struct {
	Module  string         `json:"module"`
	Rule    string         `json:"rule"`
	Verdict string         `json:"verdict"`
	Outcome string         `json:"outcome"`
	MS      jsontext.Value `json:"ms"`
}

type signalRule struct{ rule, module string }

type Report struct {
	Start     string
	Dropped   int
	Truncated bool
	modules   map[string]*moduleStats
	rules     map[string]map[signalRule]int
	totals    map[string]int
}

// Read keeps the two views on the same bounded scan and projections.
func Read(now time.Time, view View) Report {
	s := Report{
		Start:   now.AddDate(0, 0, 1-int(view)).Format(time.DateOnly),
		modules: map[string]*moduleStats{},
		rules:   map[string]map[signalRule]int{"block": {}, "ask": {}, "refused": {}},
		totals:  map[string]int{},
	}
	dir := filepath.Join(paths.StateDir(), "hooks")
	left := int64(budget)
	for day := range int(view) {
		date := now.AddDate(0, 0, -day).Format("20060102")
		used, truncated := s.readFile(filepath.Join(dir, "telemetry-"+date+".jsonl"), left, view)
		left -= used
		if truncated {
			s.Truncated = true
			break
		}
	}
	return s
}

func (s *Report) readFile(path string, left int64, view View) (used int64, truncated bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return 0, false
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	data := make([]byte, min(left, info.Size()))
	n, _ := io.ReadFull(f, data)
	s.scan(data[:n], view)
	return int64(n), int64(n) == left && info.Size() > left
}

func (s *Report) scan(data []byte, view View) {
	const workers, minChunk = 6, 256 << 10
	size := max(minChunk, len(data)/workers)
	if len(data) <= size || view == Signals {
		s.scanLines(data, view)
		return
	}
	var wg sync.WaitGroup
	var partials []*Report
	for len(data) > size {
		i := bytes.IndexByte(data[size:], '\n')
		if i < 0 {
			break
		}
		chunk := data[:size+i+1]
		data = data[len(chunk):]
		p := &Report{modules: map[string]*moduleStats{}}
		partials = append(partials, p)
		wg.Go(func() { p.scanLines(chunk, view) })
	}
	s.scanLines(data, view)
	wg.Wait()
	for _, p := range partials {
		s.mergeActivity(p)
	}
}

func (s *Report) scanLines(data []byte, view View) {
	for len(data) > 0 {
		line, rest, ok := bytes.Cut(data, []byte{'\n'})
		if !ok {
			return
		}
		if len(line) < lineCap {
			s.add(line, view)
		}
		data = rest
	}
}

func (s *Report) mergeActivity(p *Report) {
	s.Dropped += p.Dropped
	for name, next := range p.modules {
		m := s.modules[name]
		if m == nil {
			s.modules[name] = next
			continue
		}
		m.Fires += next.Fires
		m.Denies += next.Denies
		m.Overruns += next.Overruns
		m.durations = append(m.durations, next.durations...)
	}
}

func signalName(s string) bool {
	if len(s) == 0 || len(s) > 100 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case (c|32)-'a' <= 'z'-'a', c-'0' <= 9:
		case i > 0 && (c == '_' || c == '.' || c == '-'):
		default:
			return false
		}
	}
	return true
}

func (s *Report) add(line []byte, view View) {
	var row signalRow
	if jsonv2.Unmarshal(line, &row) != nil {
		return
	}
	if row.Outcome == "dropped" {
		s.Dropped++
		return
	}
	if !signalName(row.Module) {
		return
	}
	m := s.modules[row.Module]
	if m == nil {
		m = &moduleStats{}
		s.modules[row.Module] = m
	}
	m.Fires++
	if view == Activity {
		m.record(row)
		return
	}
	switch row.Verdict {
	case "block", "ask", "refused", "held", "passed", "rewrite":
		s.totals[row.Verdict]++
	}
	if counts := s.rules[row.Verdict]; counts != nil && signalName(row.Rule) {
		counts[signalRule{row.Rule, row.Module}]++
	}
}

func rankedSignals(counts map[signalRule]int) []signalRule {
	keys := make([]signalRule, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if counts[a] != counts[b] {
			return counts[a] > counts[b]
		}
		if a.rule != b.rule {
			return a.rule < b.rule
		}
		return a.module < b.module
	})
	return keys
}

func (s Report) WriteSignals(w io.Writer) {
	if len(s.modules) == 0 && s.Dropped == 0 && !s.Truncated {
		return
	}
	fmt.Fprintf(w, "signals (since %s", s.Start)
	if s.Truncated {
		fmt.Fprintf(w, "; %d MiB read limit reached", budget>>20)
	}
	fmt.Fprintln(w, "):")
	const top = 2
	for _, verdict := range []string{"block", "ask"} {
		counts := s.rules[verdict]
		keys := rankedSignals(counts)
		for _, key := range keys[:min(top, len(keys))] {
			fmt.Fprintf(w, "  %s: %s (%s) %d (%.1f%%)\n", verdict, key.rule, key.module,
				counts[key], 100*float64(counts[key])/float64(s.modules[key.module].Fires))
		}
	}
	fmt.Fprintf(w, "  hand-backs: held %d, refused %d, passed %d", s.totals["held"], s.totals["refused"], s.totals["passed"])
	counts := s.rules["refused"]
	if keys := rankedSignals(counts); len(keys) > 0 {
		key := keys[0]
		fmt.Fprintf(w, "; top refused: %s (%s) %d", key.rule, key.module, counts[key])
	}
	fmt.Fprintf(w, "\n  rewrite: %d\n", s.totals["rewrite"])
	if s.Dropped > 0 {
		fmt.Fprintf(w, "  dropped: >=%d (cap markers)\n", s.Dropped)
	}
}

type Module struct {
	Name                    string
	Fires, Denies, Overruns int
	P95                     float64
	HasDuration             bool
}

type moduleStats struct {
	Module
	durations []float64
}

func (m *moduleStats) record(row signalRow) {
	switch row.Outcome {
	case "deny", "block":
		m.Denies++
	case "overrun":
		m.Overruns++
	}
	if ms, err := strconv.ParseFloat(string(row.MS), 64); err == nil && ms >= 0 && len(row.MS) > 0 && row.MS[0] <= '9' {
		m.durations = append(m.durations, ms)
	}
}

func (s Report) Modules() []Module {
	rows := make([]Module, 0, len(s.modules))
	for name, m := range s.modules {
		row := m.Module
		row.Name = name
		if n := len(m.durations); n > 0 {
			slices.Sort(m.durations)
			row.P95, row.HasDuration = m.durations[n-1-n/20], true
		}
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b Module) int { return cmp.Compare(a.Name, b.Name) })
	return rows
}
