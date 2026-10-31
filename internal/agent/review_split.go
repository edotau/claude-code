package agent

import (
	"fmt"
	"sort"
	"strings"
)

// ReviewBudget is the most chars (prompt + diff) one review call carries; codex rejects input over 1,048,576.
const ReviewBudget = 900_000

// partOverhead reserves room for the part header and the attachment framing around each chunk.
const partOverhead = 4000

// maxHeaderFiles caps the file names listed in a part header.
const maxHeaderFiles = 30

// Part is one chunk of a split diff: its text and the files it covers.
type Part struct {
	Diff  []byte
	Files []string
}

// Header is the "part i/N, files: …" line (i is 1-based) appended to the review prompt.
func (p Part) Header(i, n int) string {
	files := p.Files
	extra := 0
	if len(files) > maxHeaderFiles {
		files, extra = files[:maxHeaderFiles], len(files)-maxHeaderFiles
	}
	h := fmt.Sprintf("part %d/%d, files: %s", i, n, strings.Join(files, ", "))
	if extra > 0 {
		h += fmt.Sprintf(", … +%d more", extra)
	}
	return h
}

type unit struct{ group, file, text string }

// SplitDiff returns diff whole when prompt+diff fits budget, else path-grouped chunks that each fit.
func SplitDiff(diff []byte, promptLen, budget int) []Part {
	if promptLen+len(diff) <= budget {
		return []Part{{Diff: diff, Files: fileList(sections(string(diff)))}}
	}
	limit := max(budget-promptLen-partOverhead, 1)
	var groups []string
	byGroup := map[string][]unit{}
	for _, u := range sections(string(diff)) {
		if _, ok := byGroup[u.group]; !ok {
			groups = append(groups, u.group)
		}
		byGroup[u.group] = append(byGroup[u.group], u)
	}
	sort.Strings(groups)

	var parts []Part
	var cur []unit
	size := 0
	flush := func() {
		if len(cur) > 0 {
			parts = append(parts, Part{Diff: []byte(joinUnits(cur)), Files: fileList(cur)})
		}
		cur, size = nil, 0
	}
	add := func(u unit) {
		if size+len(u.text) > limit {
			flush()
		}
		cur, size = append(cur, u), size+len(u.text)
	}
	for _, g := range groups {
		total := 0
		for _, u := range byGroup[g] {
			total += len(u.text)
		}
		if total <= limit { // a whole directory stays together
			if size+total > limit {
				flush()
			}
			for _, u := range byGroup[g] {
				cur, size = append(cur, u), size+len(u.text)
			}
			continue
		}
		for _, u := range byGroup[g] {
			for _, piece := range splitUnit(u, limit) {
				add(piece)
			}
		}
	}
	flush()
	return parts
}

func joinUnits(us []unit) string {
	var b strings.Builder
	for _, u := range us {
		b.WriteString(u.text)
	}
	return b.String()
}

// fileList is the distinct file names of us, in order.
func fileList(us []unit) []string {
	var out []string
	for _, u := range us {
		if len(out) == 0 || out[len(out)-1] != u.file {
			out = append(out, u.file)
		}
	}
	return out
}

// splitUnit cuts a unit over limit on hunk boundaries, repeating the file header; an oversized hunk is cut on lines.
func splitUnit(u unit, limit int) []unit {
	if len(u.text) <= limit {
		return []unit{u}
	}
	lines := strings.SplitAfter(u.text, "\n")
	headEnd := len(lines)
	for i, l := range lines {
		if strings.HasPrefix(l, "@@ ") {
			headEnd = i
			break
		}
	}
	head := strings.Join(lines[:headEnd], "")
	body := lines[headEnd:]
	if headEnd == len(lines) { // no hunks (untracked content): cut the whole text on lines
		head, body = "", lines
	}
	room := max(limit-len(head), 1)
	var hunks []string // each hunk, or a line-cut slice of one too big for room
	var h strings.Builder
	pushHunk := func() {
		for t := h.String(); t != ""; {
			n := min(len(t), room)
			if n < len(t) { // cut an oversized hunk at a line end
				if i := strings.LastIndex(t[:n], "\n"); i >= 0 {
					n = i + 1
				}
			}
			hunks, t = append(hunks, t[:n]), t[n:]
		}
		h.Reset()
	}
	for _, l := range body {
		if strings.HasPrefix(l, "@@ ") {
			pushHunk()
		}
		h.WriteString(l)
	}
	pushHunk()
	var out []unit
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			out = append(out, unit{u.group, u.file, head + b.String()})
			b.Reset()
		}
	}
	for _, hk := range hunks {
		if b.Len()+len(hk) > room {
			flush()
		}
		b.WriteString(hk)
	}
	flush()
	return out
}

// sections cuts diff into per-file units at "diff --git" and untracked "new file:" headers.
func sections(diff string) []unit {
	var out []unit
	var cur *unit
	var b strings.Builder
	end := func() {
		if cur != nil {
			cur.text = b.String()
			out = append(out, *cur)
		}
		b.Reset()
	}
	for l := range strings.SplitAfterSeq(diff, "\n") {
		if p, ok := sectionPath(l); ok {
			end()
			cur = &unit{group: groupOf(p), file: p}
		} else if cur == nil {
			cur = &unit{group: ".", file: "(preamble)"}
		}
		b.WriteString(l)
	}
	end()
	return out
}

// sectionPath extracts the file path from a section header line.
func sectionPath(l string) (string, bool) {
	l = strings.TrimRight(l, "\n")
	if rest, ok := strings.CutPrefix(l, "diff --git "); ok {
		if _, after, ok := strings.CutLast(rest, " b/"); ok {
			return after, true
		}
		return rest, true
	}
	if rest, ok := strings.CutPrefix(l, "new file: "); ok {
		p, _, _ := strings.Cut(rest, " (")
		return p, true
	}
	return "", false
}

// nestedRoots are top-level dirs whose first-level child is the group (internal/agent, cmd/claude-code).
var nestedRoots = map[string]bool{"internal": true, "cmd": true, "pkg": true, "src": true, "lib": true, "apps": true, "packages": true}

// groupOf is the top-level path group of p.
func groupOf(p string) string {
	seg := strings.Split(p, "/")
	switch {
	case len(seg) == 1:
		return "."
	case nestedRoots[seg[0]] && len(seg) > 2:
		return seg[0] + "/" + seg[1]
	default:
		return seg[0]
	}
}
