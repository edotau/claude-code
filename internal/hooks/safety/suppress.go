// Lint suppression — safety-scan's fourth rule: an edit that adds a lint or type-check suppression line.

package safety

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/platform/audit"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/textutil"
)

// suppressPatterns is the one typed table of markers (case-insensitive); each is a regexp, never a literal marker.
var suppressPatterns = []string{
	`#\s*noqa\b`, `#\s*ruff:\s*noqa\b`, `#\s*type:\s*ignore\b`, `#\s*pyright:\s*ignore\b`,
	`#\s*mypy:\s*ignore-errors\b`, `#\s*pylint:\s*(disable|skip-file)\b`, `#\s*shellcheck\s+disable\b`,
	`//\s*nolint(nextline)?\b`, `(//|/\*|#)\s*eslint-disable\b`, `(//|/\*)\s*@ts-(ignore|nocheck|expect-error)\b`,
	`#!?\[\s*allow\s*\(`, `#\s*pyre-(ignore|fixme)\b`, `(//|#)\s*#?nosec\b`, `(//|#)\s*nosemgrep\b`,
	`//\s*noinspection\b`, `@SuppressWarnings\(`, `#\s*pragma\s+(warning\s*\(\s*disable\b|(\w+\s+)?diagnostic\s+ignored\b)`,
}

var suppressMarkers = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)` + strings.Join(suppressPatterns, "|"))
})

// markerIn is the first marker on l outside a string literal, as [start, end] (nil when none); one forward pass.
func markerIn(l string) []int {
	var quote byte
	pos := 0
	for _, m := range suppressMarkers().FindAllStringIndex(l, -1) {
		var comment bool
		quote, comment, pos = scanCode(l, pos, m[0], quote)
		if quote == 0 && (!comment || l[m[0]] != '@') { // an annotation is code: it never counts in a comment
			return m
		}
	}
	return nil
}

// scanCode walks l[from:to] from string state quote: the state at the end, a comment start, where it stopped.
func scanCode(l string, from, to int, quote byte) (byte, bool, int) {
	i := from
	for ; i < to; i++ {
		c := l[i]
		if quote != 0 {
			switch c {
			case '\\':
				i++
			case quote:
				quote = 0
			}
		} else if strings.ContainsRune("\"'`", rune(c)) {
			quote = c
		} else if c == '#' || strings.HasPrefix(l[i:], "//") || strings.HasPrefix(l[i:], "/*") {
			return 0, true, i
		}
	}
	return quote, false, i
}

// suppressDocExts are prose files where a marker is text, not a comment.
var suppressDocExts = map[string]bool{".md": true, ".mdx": true}

// addition is text a call writes; known says whether a trimmed line was already there.
type addition struct {
	path, text string
	known      func(line string) bool
}

const suppressHint = "refactor so the rule passes (rules/standards/quality.md)"

// suppressVerdict judges the lines an edit call adds: ask in the main session, block in any agent.
func suppressVerdict(in *core.Input) core.Result {
	if in.ToolName == "" || strings.EqualFold(in.CanonicalTool(), "Bash") {
		return core.Result{}
	}
	for _, a := range addedTexts(in) {
		if suppressDocExts[strings.ToLower(filepath.Ext(a.path))] || !suppressMarkers().MatchString(a.text) {
			continue
		}
		lines := strings.Split(a.text, "\n")
		for i, l := range lines {
			m := markerIn(l)
			if m == nil || a.known(strings.TrimSpace(l)) {
				continue
			}
			where := fmt.Sprintf("%s, %s:%d", l[m[0]:m[1]], a.path, i+1)
			return suppressResult(in, where, l, suppressReason(lines, i, m[1]))
		}
	}
	return core.Result{}
}

// isMainSession is false in a subagent (agent_id) and in any dispatched agent (the dispatch id env).
func isMainSession(in *core.Input) bool {
	return in.AgentID == "" && os.Getenv(proc.DispatchIDEnv) == ""
}

// suppressResult asks in the main session (a rare exception) and blocks in every agent.
func suppressResult(in *core.Input, where, line, reason string) core.Result {
	main := isMainSession(in)
	audit.LogSession(audit.HookBlocked, in.SessionID, "safety", "lint suppression")
	if main && core.SupportsAsk(in.Agent()) {
		if reason == "" {
			reason = "no reason given on the line"
		}
		return core.Result{Decision: core.Ask, Floor: true, Rule: "ask: lint suppression", Reason: fmt.Sprintf(
			"lint suppression: refactor is the default; approve only if the fix is disproportionate (e.g. a one-line "+
				"typing gap). %s: %q; reason: %s", where, strings.TrimSpace(line), reason)}
	}
	return core.Result{Decision: core.Block, Floor: true, Rule: "lint suppression", Reason: fmt.Sprintf(
		"lint suppression added (%s): %s; if the refactor is disproportionate, hand the line back in UNFIXED "+
			"with its reason so the main thread can decide", where, suppressHint)}
}

// suppressReason is the comment after the marker on lines[i], else the comment line above it ("" if none).
func suppressReason(lines []string, i, end int) string {
	rest := lines[i][end:]
	for _, lead := range []string{"#", "//"} {
		if _, after, ok := strings.Cut(rest, lead); ok && strings.TrimSpace(after) != "" {
			return strings.TrimSpace(after)
		}
	}
	if i == 0 {
		return ""
	}
	above := strings.TrimSpace(lines[i-1])
	if (strings.HasPrefix(above, "#") || strings.HasPrefix(above, "//")) && !suppressMarkers().MatchString(above) {
		return strings.TrimSpace(strings.TrimLeft(above, "#/"))
	}
	return ""
}

// addedTexts are the texts a file-edit call writes, from apply_patch bodies or a single-file tool's arguments.
func addedTexts(in *core.Input) []addition {
	edits := in.EditedPaths()
	out := patchAdditions(edits, in.CWD)
	ti := in.ToolInput()
	if len(out) > 0 || ti == nil {
		return out
	}
	path := strField(ti, "notebook_path")
	if len(edits) > 0 {
		path = edits[0].Path
	}
	if path == "" {
		return nil
	}
	return singleFileAdditions(paths.AnchorPath(path, in.CWD), ti)
}

// patchAdditions are the apply_patch bodies; an update's lines, and those of a rename from it, may already exist.
func patchAdditions(edits []textutil.FileEdit, cwd string) []addition {
	var out []addition
	source := ""
	for _, e := range edits {
		path := paths.AnchorPath(e.Path, cwd)
		known := func(string) bool { return false }
		switch e.Kind {
		case textutil.PatchUpdate:
			source, known = path, existingLines(path)
		case textutil.PatchAdd:
			known = existingLines(path)
		case textutil.PatchMove:
			known = existingLines(source)
		}
		if e.Body != "" {
			out = append(out, addition{path, e.Body, known})
		}
	}
	return out
}

func strField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, _ := m[k].(string); s != "" {
			return s
		}
	}
	return ""
}

// singleFileAdditions reads Write content, a notebook cell and Edit or MultiEdit replacements.
func singleFileAdditions(path string, ti map[string]any) []addition {
	var out []addition
	if s := strField(ti, "content"); s != "" {
		if cells, ok := notebookCodeAdditions(path, s); ok {
			out = append(out, cells...)
		} else {
			out = append(out, addition{path, s, existingLines(path)})
		}
	}
	var src string
	if parts, ok := ti["new_source"].([]any); ok { // a source-line array
		var sb strings.Builder
		for _, p := range parts {
			s, _ := p.(string)
			sb.WriteString(strings.TrimSuffix(s, "\n"))
			sb.WriteByte('\n')
		}
		src = sb.String()
	} else {
		src = strField(ti, "new_source")
	}
	if src != "" && !isProseMagic(src) {
		out = append(out, addition{path, src, existingText(path)})
	}
	pairs, _ := ti["edits"].([]any)
	for _, p := range append(pairs, ti) {
		m, _ := p.(map[string]any)
		if s := strField(m, "new_string", "newString"); s != "" {
			old, file := lineSet(strField(m, "old_string", "oldString")), existingLines(path)
			out = append(out, addition{path, s, func(l string) bool { return old(l) || file(l) }})
		}
	}
	return out
}

func lineSet(s string) func(string) bool {
	set := map[string]bool{}
	for l := range strings.SplitSeq(s, "\n") {
		set[strings.TrimSpace(l)] = true
	}
	return func(l string) bool { return set[l] }
}

// existingLines reads path lazily (only a line carrying a marker asks); a missing file has no known lines.
func existingLines(path string) func(string) bool {
	var known func(string) bool
	return func(l string) bool {
		if known == nil {
			b, _ := os.ReadFile(path)
			known = lineSet(string(b))
		}
		return known(l)
	}
}

// existingText is the set of source lines across a notebook's cells (source is a string or a list of strings).
func existingText(path string) func(string) bool {
	var known func(string) bool
	return func(l string) bool {
		if known == nil {
			var nb struct {
				Cells []struct {
					Source json.RawMessage `json:"source"`
				} `json:"cells"`
			}
			b, _ := os.ReadFile(path)
			_ = json.Unmarshal(b, &nb) // a missing or broken notebook has no known lines
			var sb strings.Builder
			for _, c := range nb.Cells {
				var one string
				var many []string
				if json.Unmarshal(c.Source, &one) == nil {
					sb.WriteString(one)
					sb.WriteByte('\n')
				} else if json.Unmarshal(c.Source, &many) == nil {
					for _, s := range many {
						sb.WriteString(s)
					}
					sb.WriteByte('\n')
				}
			}
			known = lineSet(sb.String())
		}
		return known(l)
	}
}

// nbCell is the part of a notebook cell the scan reads: its type and its source (a string or a list of lines).
type nbCell struct {
	CellType string          `json:"cell_type"`
	Source   json.RawMessage `json:"source"`
}

// isProseMagic is a notebook cell whose cell magic renders prose (%%markdown, %%html), not code.
func isProseMagic(src string) bool {
	head := strings.TrimLeft(src, " \t\r\n")
	return strings.HasPrefix(head, "%%markdown") || strings.HasPrefix(head, "%%html")
}

// notebookCodeAdditions are an .ipynb Write body's code-cell sources, checked like NotebookEdit's; false otherwise.
func notebookCodeAdditions(path, body string) ([]addition, bool) {
	if !strings.EqualFold(filepath.Ext(path), ".ipynb") {
		return nil, false
	}
	var nb struct {
		Cells *[]nbCell `json:"cells"`
	}
	if json.Unmarshal([]byte(body), &nb) != nil || nb.Cells == nil {
		return nil, false
	}
	var out []addition
	known := existingText(path)
	for _, c := range *nb.Cells {
		if c.CellType != "code" {
			continue
		}
		var one string
		var many []string
		switch {
		case json.Unmarshal(c.Source, &one) == nil:
		case json.Unmarshal(c.Source, &many) == nil:
			one = strings.Join(many, "")
		default:
			return nil, false
		}
		if isProseMagic(one) {
			continue
		}
		out = append(out, addition{path, one, known})
	}
	return out, true
}
