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

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/textutil"
)

// suppressPatterns is the one typed table of markers (case-insensitive); each is a regexp, never a literal marker.
var suppressPatterns = []string{
	`#\s*noqa\b`, `#\s*ruff:\s*noqa\b`, `#\s*type:\s*ignore\b`, `#\s*pyright:\s*ignore\b`,
	`#\s*mypy:\s*ignore-errors\b`, `#\s*pylint:\s*(disable|skip-file)\b`, `#\s*shellcheck\s+disable\b`,
	`//\s*nolint(nextline)?\b`, `eslint-disable\b`, `@ts-(ignore|nocheck|expect-error)\b`, `#!?\[\s*allow\s*\(`,
}

var suppressMarkers = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)` + strings.Join(suppressPatterns, "|"))
})

// markerIn is the first marker on l outside a double-quoted string, as [start, end] ("" text when none).
func markerIn(l string) []int {
	for _, m := range suppressMarkers().FindAllStringIndex(l, -1) {
		if strings.Count(l[:m[0]], `"`)%2 == 0 {
			return m
		}
	}
	return nil
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
		if e.Body == "" {
			continue
		}
		path, known := paths.AnchorPath(e.Path, in.CWD), func(string) bool { return false }
		if e.Kind == textutil.PatchUpdate {
			known = existingLines(path)
		}
		out = append(out, addition{path, e.Body, known})
	}
	ti := in.ToolInput()
	if len(out) > 0 || ti == nil {
		return out
	}
	path := ""
	if len(edits) > 0 {
		path = edits[0].Path
	} else if nb := strField(ti, "notebook_path"); nb != "" {
		path = paths.AnchorPath(nb, in.CWD)
	}
	if path == "" {
		return nil
	}
	return singleFileAdditions(path, ti)
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
					sb.WriteString(one + "\n")
				} else if json.Unmarshal(c.Source, &many) == nil {
					sb.WriteString(strings.Join(many, "") + "\n")
				}
			}
			known = lineSet(sb.String())
		}
		return known(l)
	}
}
