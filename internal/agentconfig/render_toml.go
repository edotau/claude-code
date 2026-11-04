package agentconfig

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
)

// TOMLString reads one basic or literal TOML string value, dropping a trailing comment; bare values are kept.
func TOMLString(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 0 && (v[0] == '"' || v[0] == '\'') {
		if v[0] == '"' {
			if end := basicStringEnd(v); end > 0 {
				if s, err := strconv.Unquote(v[:end+1]); err == nil {
					return s
				}
				return v[1:end]
			}
		} else if end := strings.IndexByte(v[1:], '\''); end >= 0 {
			return v[1 : end+1]
		}
		return strings.Trim(v, `"'`)
	}
	v, _, _ = strings.Cut(v, "#")
	return strings.TrimSpace(v)
}

// basicStringEnd is the index of the closing quote of the basic string opening v, skipping escapes; -1 if unclosed.
func basicStringEnd(v string) int {
	for i := 1; i < len(v); i++ {
		switch v[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

// TOMLQuote renders s as a TOML basic string (a JSON string is one) for a codex -c override.
func TOMLQuote(s string) string {
	b, _ := json.Marshal(s) // a JSON string is a valid TOML basic string
	return string(b)
}

func TOMLInline(pairs [][2]string) string {
	parts := make([]string, len(pairs))
	for i, kv := range pairs {
		parts[i] = kv[0] + " = " + kv[1]
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

func AppendTOML(raw, section string) string {
	if raw == "" {
		return section
	}
	if !strings.HasSuffix(raw, "\n") {
		raw += "\n"
	}
	return raw + "\n" + section
}

// InsertTopLevelTOML puts section before the first real top-level table header (outside multiline
// strings and open arrays), dropping any pre-existing top-level assignment section itself also sets,
// so TOML never ends up with duplicate keys; with no such header in raw, it falls back to AppendTOML.
func InsertTopLevelTOML(raw, section string) string {
	keys := topLevelKeysOf(section)
	lines := strings.SplitAfter(raw, "\n")
	idx := FirstTopLevelHeaderIndex(lines)
	if idx == -1 {
		return AppendTOML(dropTopLevelKeys(raw, keys), section)
	}
	before := dropTopLevelKeys(strings.Join(lines[:idx], ""), keys)
	before = strings.TrimRight(before, "\n")
	after := strings.Join(lines[idx:], "")
	if before != "" {
		before += "\n\n"
	}
	return before + section + "\n" + after
}

// topLevelKeysOf returns the bare key names section assigns before its own first table header, i.e.
// the keys an insert must not duplicate in whatever top-level text already precedes it.
func topLevelKeysOf(section string) []string {
	lines := strings.SplitAfter(section, "\n")
	idx := FirstTopLevelHeaderIndex(lines)
	if idx == -1 {
		idx = len(lines)
	}
	var keys []string
	multiline, depth := "", 0
	for _, line := range lines[:idx] {
		trimmed := strings.TrimSpace(line)
		if multiline == "" && depth == 0 && trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			if key, _, ok := strings.Cut(trimmed, "="); ok {
				keys = append(keys, strings.TrimSpace(key))
			}
		}
		multiline, depth = TOMLLineState(line, multiline, depth)
	}
	return keys
}

// TOMLKeyName decodes one quoted key without stripping quotes that belong to its actual name.
func TOMLKeyName(raw string) string {
	key := strings.TrimSpace(raw)
	if len(key) >= 2 && key[0] == '\'' && key[len(key)-1] == '\'' {
		return key[1 : len(key)-1]
	}
	if strings.HasPrefix(key, "\"") {
		if value, err := strconv.Unquote(key); err == nil {
			return value
		}
	}
	return key
}

// dropTopLevelKeys removes whole lines (including multiline/array continuations) that assign one of
// keys at true top level (depth 0, outside multiline strings), leaving same-named keys inside a table.
func dropTopLevelKeys(raw string, keys []string) string {
	if len(keys) == 0 || raw == "" {
		return raw
	}
	var out strings.Builder
	multiline, depth := "", 0
	skipping := false
	for _, line := range strings.SplitAfter(raw, "\n") {
		drop := skipping || (multiline == "" && depth == 0 && assignsKey(line, keys))
		nextMultiline, nextDepth := TOMLLineState(line, multiline, depth)
		if drop {
			skipping = nextMultiline != "" || nextDepth > 0
		} else {
			out.WriteString(line)
		}
		multiline, depth = nextMultiline, nextDepth
	}
	return out.String()
}

// assignsKey reports whether line is a "key = ..." assignment of one of keys.
func assignsKey(line string, keys []string) bool {
	key, _, ok := strings.Cut(strings.TrimSpace(line), "=")
	return ok && slices.Contains(keys, TOMLKeyName(key))
}

// FirstTopLevelHeaderIndex finds the first line whose "[" is a genuine top-level table header: outside
// any multiline string and outside an open (possibly multiline) array; -1 when raw has none.
func FirstTopLevelHeaderIndex(lines []string) int {
	multiline, depth := "", 0
	for i, line := range lines {
		if multiline == "" && depth == 0 && strings.HasPrefix(strings.TrimSpace(line), "[") {
			return i
		}
		multiline, depth = TOMLLineState(line, multiline, depth)
	}
	return -1
}

// TOMLLineState extends tomlMultilineState with bracket depth, so a "["-led continuation line inside
// an open (possibly multiline) array is never mistaken for a table header.
func TOMLLineState(line, state string, depth int) (string, int) {
	for i := 0; i < len(line); {
		if state != "" {
			i, state = stepInMultiline(line, i, state)
			continue
		}
		if line[i] == '#' {
			return state, depth
		}
		i, state, depth = stepOutsideString(line, i, depth)
	}
	return state, depth
}

// stepInMultiline advances one step through a multiline string, closing it at its terminator.
func stepInMultiline(line string, i int, state string) (int, string) {
	if state == `"""` && line[i] == '\\' {
		return i + 2, state
	}
	if strings.HasPrefix(line[i:], state) {
		return i + 3, ""
	}
	return i + 1, state
}

// stepOutsideString advances one step outside any string, tracking bracket depth and skipping strings.
func stepOutsideString(line string, i, depth int) (int, string, int) {
	switch line[i] {
	case '[':
		return i + 1, "", depth + 1
	case ']':
		if depth > 0 {
			depth--
		}
		return i + 1, "", depth
	case '"', '\'':
		quote := line[i]
		triple := strings.Repeat(string(quote), 3)
		if strings.HasPrefix(line[i:], triple) {
			return i + 3, triple, depth
		}
		return skipQuoted(line, i+1, quote), "", depth
	}
	return i + 1, "", depth
}

// skipQuoted returns the index just past the single-line string closing quote, honoring "-string escapes.
func skipQuoted(line string, i int, quote byte) int {
	for i < len(line) {
		switch {
		case line[i] == '\\' && quote == '"':
			i += 2
		case line[i] == quote:
			return i + 1
		default:
			i++
		}
	}
	return i
}
