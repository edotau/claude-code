// promptUsage counts, over recent session transcripts, which listed skills, agents and MCP servers were never used.

package doctor

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	usageMaxSessions = 300
	usageWindow      = 7 * 24 * time.Hour
	usageBudget      = 3 * time.Second
	usageMaxRows     = 12
	usageLineCap     = 8 << 20
)

// usageStat is one listed item: sessions that loaded it, sessions that invoked it, and its listing chars.
type usageStat struct {
	loaded, used, chars int
}

var usageKinds = []string{"skill", "agent", "mcp"}

type usageSession struct {
	loaded map[string]int // kind+"\x00"+name -> listing chars
	used   map[string]bool
}

type usageLine struct {
	Attachment struct {
		Type        string   `json:"type"`
		Content     string   `json:"content"`
		AddedLines  []string `json:"addedLines"`
		AddedNames  []string `json:"addedNames"`
		AddedBlocks []string `json:"addedBlocks"`
	} `json:"attachment"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type usageToolUse struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Input struct {
		Skill        string `json:"skill"`
		SubagentType string `json:"subagent_type"`
	} `json:"input"`
}

// listingEntries splits "- name: text" listing lines into name -> chars; continuation lines join the entry.
func listingEntries(text string) map[string]int {
	out := map[string]int{}
	cur := ""
	for l := range strings.SplitSeq(text, "\n") {
		if rest, ok := strings.CutPrefix(l, "- "); ok {
			cur = rest
			if i := strings.IndexAny(cur, ": ("); i > 0 {
				cur = cur[:i]
			}
			out[cur] += len(l)
			continue
		}
		if cur != "" {
			out[cur] += len(l) + 1
		}
	}
	return out
}

func (s *usageSession) add(kind string, entries map[string]int) {
	for n, c := range entries {
		s.loaded[kind+"\x00"+n] = c
	}
}

func (s *usageSession) read(line []byte) {
	var u usageLine
	if json.Unmarshal(line, &u) != nil {
		return
	}
	s.listing(u)
	var blocks []usageToolUse
	if len(u.Message.Content) > 0 && u.Message.Content[0] == '[' && json.Unmarshal(u.Message.Content, &blocks) == nil {
		for _, b := range blocks {
			s.toolUse(b)
		}
	}
}

func (s *usageSession) listing(u usageLine) {
	a := u.Attachment
	switch a.Type {
	case "skill_listing":
		s.add("skill", listingEntries(a.Content))
	case "agent_listing_delta":
		s.add("agent", listingEntries(strings.Join(a.AddedLines, "\n")))
	case "mcp_instructions_delta":
		e := map[string]int{}
		for i, n := range a.AddedNames {
			if i < len(a.AddedBlocks) {
				e[n] = len(a.AddedBlocks[i])
			}
		}
		s.add("mcp", e)
	}
}

func (s *usageSession) toolUse(b usageToolUse) {
	switch {
	case b.Type != "tool_use":
	case b.Name == "Skill":
		s.used["skill\x00"+b.Input.Skill] = true
		_, short, _ := strings.Cut(b.Input.Skill, ":")
		s.used["skill\x00"+short] = true
	case b.Name == "Agent" || b.Name == "Task":
		s.used["agent\x00"+b.Input.SubagentType] = true
	case strings.HasPrefix(b.Name, "mcp__"):
		srv, _, _ := strings.Cut(strings.TrimPrefix(b.Name, "mcp__"), "__")
		s.used["mcp\x00"+srv] = true
	}
}

func readUsageSession(path string) (*usageSession, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := &usageSession{loaded: map[string]int{}, used: map[string]bool{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), usageLineCap)
	for sc.Scan() {
		b := sc.Bytes()
		if bytes.Contains(b, []byte(`"tool_use"`)) || bytes.Contains(b, []byte(`_listing`)) || bytes.Contains(b, []byte(`mcp_instructions_delta`)) {
			s.read(b)
		}
	}
	return s, nil // a line over the cap ends the scan; counts so far stand
}

// recentSessions lists top-level transcripts changed within window, newest first, at most limit.
func recentSessions(dir string, now time.Time, window time.Duration, limit int) []string {
	files, _ := filepath.Glob(filepath.Join(dir, "*", "*.jsonl"))
	type fm struct {
		p string
		t time.Time
	}
	var all []fm
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil && now.Sub(fi.ModTime()) <= window {
			all = append(all, fm{f, fi.ModTime()})
		}
	}
	slices.SortFunc(all, func(a, b fm) int { return b.t.Compare(a.t) })
	var out []string
	for _, x := range all[:min(len(all), limit)] {
		out = append(out, x.p)
	}
	return out
}

// usageReport scans recent sessions under dir and returns the utilization lines, each under 80 columns.
func usageReport(dir string, now time.Time, budget time.Duration) []string {
	stats := map[string]*usageStat{}
	deadline := time.Now().Add(budget)
	n := 0
	for _, p := range recentSessions(dir, now, usageWindow, usageMaxSessions) {
		if time.Now().After(deadline) {
			break
		}
		s, err := readUsageSession(p)
		if err != nil {
			continue
		}
		n++
		for k, c := range s.loaded {
			st := stats[k]
			if st == nil {
				st = &usageStat{}
				stats[k] = st
			}
			st.loaded++
			st.chars += c
			if s.used[k] {
				st.used++
			}
		}
	}
	out := []string{fmt.Sprintf("utilization: %d sessions, 7 days (tokens = chars/4)", n)}
	for _, kind := range usageKinds {
		out = append(out, unusedRows(kind, stats)...)
	}
	return out
}

func unusedRows(kind string, stats map[string]*usageStat) []string {
	type row struct {
		name string
		st   usageStat
	}
	var rows []row
	total := 0
	for k, st := range stats {
		if name, ok := strings.CutPrefix(k, kind+"\x00"); ok {
			total++
			if st.used == 0 {
				rows = append(rows, row{name, *st})
			}
		}
	}
	slices.SortFunc(rows, func(a, b row) int {
		if c := b.st.chars/b.st.loaded - a.st.chars/a.st.loaded; c != 0 {
			return c
		}
		return strings.Compare(a.name, b.name)
	})
	out := []string{fmt.Sprintf("  %s: %d never used of %d listed", kind, len(rows), total)}
	for _, r := range rows[:min(len(rows), usageMaxRows)] {
		name := r.name
		if len(name) > 40 {
			name = name[:37] + "..."
		}
		out = append(out, fmt.Sprintf("    %-40s ~%d tok x %d sess", name, r.st.chars/r.st.loaded/4, r.st.loaded))
	}
	if len(rows) > usageMaxRows {
		out = append(out, fmt.Sprintf("    ... %d more", len(rows)-usageMaxRows))
	}
	return out
}
