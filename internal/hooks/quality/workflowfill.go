package quality

import (
	"fmt"
	"sort"
	"strings"
)

// argSpan is one top-level call argument, as a [start,end) byte range relative to the args substring.
type argSpan struct {
	start, end int
}

// splitTopLevelArgs splits a call's argument-list text at top-level commas; stripStrings blanks string, template,
// regex and comment bodies first, so a comma or bracket in those never splits.
func splitTopLevelArgs(s string) []argSpan {
	var spans []argSpan
	depth := 0
	start := 0
	masked := stripStrings(s)
	for i := 0; i < len(masked); i++ {
		switch masked[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				spans = append(spans, argSpan{start, i})
				start = i + 1
			}
		}
	}
	return append(spans, argSpan{start, len(s)})
}

type fillEdit struct {
	at   int
	text string
}

const (
	fillSkip      = iota // already pinned
	fillEditing          // e holds the insertion
	fillAmbiguous        // options argument is not a plain object literal
)

// planFill decides how the agent() call at i (with its call window) gets a model pin.
func planFill(src, model string, i int, window string) (fillEdit, int) {
	if hasTopLevelKey(window, "model") {
		return fillEdit{}, fillSkip // agentType alone is not a pin — Explore runs opus
	}
	if len(window) < 6 || window[len(window)-1] != ')' {
		return fillEdit{}, fillAmbiguous
	}
	argsStart := i + strings.IndexByte(window, '(') + 1
	argsEnd := i + len(window) - 1
	argsSrc := src[argsStart:argsEnd]
	spans := splitTopLevelArgs(argsSrc)
	if len(spans) == 1 && strings.TrimSpace(argsSrc) == "" {
		return fillEdit{}, fillAmbiguous
	}
	switch len(spans) {
	case 1:
		return fillEdit{argsEnd, fmt.Sprintf(", { model: '%s' }", model)}, fillEditing
	case 2:
		return fillOptions(src, model, argsStart, argsEnd, spans[1])
	}
	return fillEdit{}, fillAmbiguous
}

// fillOptions inserts the pin into an existing options-object second argument.
func fillOptions(src, model string, argsStart, argsEnd int, span argSpan) (fillEdit, int) {
	opt := src[argsStart+span.start : argsStart+span.end]
	trimmed := skipLeadingComments(opt)
	if trimmed == "" || trimmed[0] != '{' || strings.Contains(stripStrings(trimmed), "...") {
		return fillEdit{}, fillAmbiguous
	}
	braceAt := argsStart + span.start + (len(opt) - len(trimmed))
	rest := strings.TrimLeft(src[braceAt+1:argsEnd], " \t\n")
	if strings.HasPrefix(rest, "}") {
		return fillEdit{braceAt + 1, fmt.Sprintf("model: '%s'", model)}, fillEditing
	}
	return fillEdit{braceAt + 1, fmt.Sprintf(" model: '%s',", model)}, fillEditing
}

// skipLeadingComments drops leading whitespace and /* */ or // comments, e.g. "/* c */ { ... }" → "{ ... }".
func skipLeadingComments(s string) string {
	for {
		s = strings.TrimLeft(s, " \t\r\n")
		switch {
		case strings.HasPrefix(s, "/*"):
			end := strings.Index(s, "*/")
			if end < 0 {
				return s
			}
			s = s[end+2:]
		case strings.HasPrefix(s, "//"):
			end := strings.IndexByte(s, '\n')
			if end < 0 {
				return ""
			}
			s = s[end+1:]
		default:
			return s
		}
	}
}

// FillModel rewrites every agent() call in src missing a model: pin (agentType alone is not one; opus-ok does NOT
// exempt this) to carry model: '<model>', inserted into an existing options-object 2nd argument, or
// appended as a new one. Edits apply back-to-front so offsets stay valid; a call whose options
// argument isn't a plain object literal (identifier, spread, ternary, call, or an unbalanced window)
// is left alone and its line reported in ambiguous instead. src is returned unchanged if nothing
// needed filling.
func FillModel(src, model string) (string, []string) {
	var edits []fillEdit
	var ambiguous []string
	scanAgentCalls(src, func(i, line int, window string, _ bool) {
		e, status := planFill(src, model, i, window)
		switch status {
		case fillEditing:
			edits = append(edits, e)
		case fillAmbiguous:
			ambiguous = append(ambiguous, fmt.Sprintf("line %d", line))
		}
	})
	if len(edits) == 0 {
		return src, ambiguous
	}
	sort.Slice(edits, func(a, b int) bool { return edits[a].at > edits[b].at })
	out := src
	for _, e := range edits {
		out = out[:e.at] + e.text + out[e.at:]
	}
	return out, ambiguous
}
