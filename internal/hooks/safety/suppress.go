// Lint suppression — safety-scan's fourth rule: an edit that adds a lint or type-check suppression line.

package safety

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/proc"
)

// suppressPatterns is the one typed table of markers (case-insensitive); each is a regexp, never a literal marker.
var suppressPatterns = []string{
	`#\s*noqa\b`, `#\s*ruff:\s*noqa`, `#\s*type:\s*ignore`, `#\s*pyright:\s*ignore`, `#\s*mypy:\s*ignore-errors`,
	`#\s*pylint:\s*(disable|skip-file)`, `#\s*shellcheck\s+disable`, `//\s*nolint`, `eslint-disable`,
	`@ts-(ignore|nocheck|expect-error)`, `#!?\[allow\(`,
}

var suppressMarkers = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)` + strings.Join(suppressPatterns, "|"))
})

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
			m := suppressMarkers().FindStringIndex(l)
			if m == nil || a.known(strings.TrimSpace(l)) {
				continue
			}
			where := fmt.Sprintf("%s, %s:%d", l[m[0]:m[1]], a.path, i+1)
			return suppressResult(in, where, l, suppressReason(lines, i, m[1]))
		}
	}
	return core.Result{}
}

// suppressResult asks in the main session (a rare exception) and blocks in every agent.
func suppressResult(in *core.Input, where, line, reason string) core.Result {
	main := in.AgentID == "" && os.Getenv(proc.DispatchIDEnv) == ""
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
	var out []addition
	for _, e := range edits {
		if e.Body != "" {
			out = append(out, addition{e.Path, e.Body, func(string) bool { return false }})
		}
	}
	if ti := in.ToolInput(); len(out) == 0 && len(edits) > 0 && ti != nil {
		return singleFileAdditions(edits[0].Path, ti)
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
		out = append(out, addition{path, s, existingLines(path)})
	}
	if s := strField(ti, "new_source"); s != "" {
		out = append(out, addition{path, s, existingText(path)})
	}
	pairs, _ := ti["edits"].([]any)
	for _, p := range append(pairs, ti) {
		m, _ := p.(map[string]any)
		if s := strField(m, "new_string", "newString"); s != "" {
			out = append(out, addition{path, s, lineSet(strField(m, "old_string", "oldString"))})
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

// existingText is a notebook's raw JSON, where a cell line sits inside a quoted string.
func existingText(path string) func(string) bool {
	var raw string
	var read bool
	return func(l string) bool {
		if !read {
			b, _ := os.ReadFile(path)
			raw, read = string(b), true
		}
		return strings.Contains(raw, l)
	}
}
