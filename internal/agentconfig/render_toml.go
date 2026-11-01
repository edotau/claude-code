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
		if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
			return v[1 : end+1]
		}
		return strings.Trim(v, `"'`)
	}
	v, _, _ = strings.Cut(v, "#")
	return strings.TrimSpace(v)
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

func appendTOML(raw, section string) string {
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
// so TOML never ends up with duplicate keys; with no such header in raw, it falls back to appendTOML.
func InsertTopLevelTOML(raw, section string) string {
	keys := topLevelKeysOf(section)
	lines := strings.SplitAfter(raw, "\n")
	idx := FirstTopLevelHeaderIndex(lines)
	if idx == -1 {
		return appendTOML(dropTopLevelKeys(raw, keys), section)
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
	for _, line := range lines[:idx] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if key, _, ok := strings.Cut(trimmed, "="); ok {
			keys = append(keys, strings.TrimSpace(key))
		}
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
	lines := strings.SplitAfter(raw, "\n")
	var out strings.Builder
	multiline, depth := "", 0
	skipping := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		drop := skipping
		if !skipping && multiline == "" && depth == 0 {
			if key, _, ok := strings.Cut(trimmed, "="); ok && slices.Contains(keys, TOMLKeyName(key)) {
				drop = true
			}
		}
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
			if state == `"""` && line[i] == '\\' {
				i += 2
			} else if strings.HasPrefix(line[i:], state) {
				i += 3
				state = ""
			} else {
				i++
			}
			continue
		}
		switch {
		case line[i] == '#':
			return state, depth
		case line[i] == '[':
			depth++
			i++
		case line[i] == ']':
			if depth > 0 {
				depth--
			}
			i++
		case line[i] != '"' && line[i] != '\'':
			i++
		default:
			quote := line[i]
			triple := strings.Repeat(string(quote), 3)
			if strings.HasPrefix(line[i:], triple) {
				state = triple
				i += 3
				continue
			}
			i++
			for i < len(line) {
				if line[i] == '\\' && quote == '"' {
					i += 2
				} else if line[i] == quote {
					i++
					break
				} else {
					i++
				}
			}
		}
	}
	return state, depth
}
