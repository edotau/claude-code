package agentconfig

import (
	"encoding/json"
	"errors"
	"iter"
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
	var keys []string
	for l := range tomlLines(strings.SplitAfter(section, "\n")) {
		if !l.Top {
			continue
		}
		trimmed := strings.TrimSpace(l.Text)
		if strings.HasPrefix(trimmed, "[") {
			break
		}
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			if key, _, ok := strings.Cut(trimmed, "="); ok {
				keys = append(keys, TOMLKeyName(key))
			}
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
	var out strings.Builder
	skipping := false
	for l := range tomlLines(strings.SplitAfter(raw, "\n")) {
		if skipping || (l.Top && assignsKey(l.Text, keys)) {
			skipping = l.Open
		} else {
			out.WriteString(l.Text)
		}
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
	for l := range tomlLines(lines) {
		if l.Top && strings.HasPrefix(strings.TrimSpace(l.Text), "[") {
			return l.Index
		}
	}
	return -1
}

// tomlLine is one line with its string/array state: Top before it (outside multiline strings and arrays), Open after.
type tomlLine struct {
	Index int
	Text  string
	Top   bool
	Open  bool
}

// tomlLines yields every line in order with the multiline-string and bracket-depth state around it.
func tomlLines(lines []string) iter.Seq[tomlLine] {
	return func(yield func(tomlLine) bool) {
		multiline, depth := "", 0
		for i, text := range lines {
			top := multiline == "" && depth == 0
			multiline, depth = TOMLLineState(text, multiline, depth)
			if !yield(tomlLine{Index: i, Text: text, Top: top, Open: multiline != "" || depth > 0}) {
				return
			}
		}
	}
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
		if triple := line[i:min(i+3, len(line))]; len(triple) == 3 && triple[1] == quote && triple[2] == quote {
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

// ErrOpenString reports a multiline string whose terminator is not in the value yet.
var ErrOpenString = errors.New("unterminated multiline string")

// tripleString parses a TOML triple-quoted string at the start of value, trimming the first newline.
func tripleString(value string) (parsed, rest string, err error) {
	quote := value[:3]
	end := strings.Index(value[3:], quote)
	if end < 0 {
		return "", "", ErrOpenString
	}
	body := strings.TrimPrefix(strings.TrimPrefix(value[3:3+end], "\r"), "\n")
	rest = value[3+end+3:]
	if quote == "\"\"\"" {
		body = strings.ReplaceAll(strings.ReplaceAll(body, "\r", ""), "\n", `\n`)
		if body, err = strconv.Unquote(`"` + strings.ReplaceAll(body, `"`, `\"`) + `"`); err != nil {
			return "", "", err
		}
	}
	return body, rest, nil
}

// TOMLStringValue strictly parses one basic, literal or triple-quoted string value, allowing a trailing comment.
func TOMLStringValue(value string) (string, error) {
	var parsed, rest string
	var err error
	switch {
	case strings.HasPrefix(value, "\"\"\"") || strings.HasPrefix(value, "'''"):
		parsed, rest, err = tripleString(value)
	case strings.HasPrefix(value, "'"):
		parsed, rest, err = literalString(value)
	default:
		parsed, rest, err = basicString(value)
	}
	if err != nil {
		return "", err
	}
	if rest = strings.TrimSpace(rest); rest != "" && !strings.HasPrefix(rest, "#") {
		return "", errors.New("unexpected text after string")
	}
	return parsed, nil
}

// literalString parses a single-quoted TOML literal string at the start of value.
func literalString(value string) (parsed, rest string, err error) {
	end := strings.IndexByte(value[1:], '\'')
	if end < 0 {
		return "", "", errors.New("unterminated literal string")
	}
	return value[1 : end+1], value[end+2:], nil
}

// basicString parses a double-quoted TOML string at the start of value.
func basicString(value string) (parsed, rest string, err error) {
	quoted, err := strconv.QuotedPrefix(value)
	if err != nil || !strings.HasPrefix(quoted, "\"") {
		return "", "", errors.New("expected quoted string")
	}
	if parsed, err = strconv.Unquote(quoted); err != nil {
		return "", "", err
	}
	return parsed, value[len(quoted):], nil
}

// TOMLTableName is the name of a `[name]` table header line, a trailing comment allowed; "" when not a header.
func TOMLTableName(line string) string {
	if !strings.HasPrefix(line, "[") || strings.HasPrefix(line, "[[") {
		return ""
	}
	end := headerClose(line)
	if end < 0 {
		return ""
	}
	if rest := strings.TrimSpace(line[end+1:]); rest != "" && !strings.HasPrefix(rest, "#") {
		return ""
	}
	return strings.TrimSpace(line[1:end])
}

// headerClose is the index of the `]` closing a table header, skipping any `]` inside quoted key parts; -1 if none.
func headerClose(line string) int {
	var quote byte // open key quote (" or '), 0 outside quotes
	escaped := false
	for i := 1; i < len(line); i++ {
		c := line[i]
		switch {
		case quote == '"' && escaped:
			escaped = false
		case quote == '"' && c == '\\':
			escaped = true
		case quote != 0 && c == quote:
			quote = 0
		case quote != 0:
		case c == '"' || c == '\'':
			quote = c
		case c == ']':
			return i
		}
	}
	return -1
}

// SplitInlineTable splits top-level TOML inline-table commas, ignoring quoted strings.
func SplitInlineTable(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var parts []string
	depth, start := 0, 0
	var quote rune // the open string delimiter (" or '), 0 outside a string
	escaped := false
	for i, r := range s {
		if quote != 0 {
			switch {
			case escaped:
				escaped = false
			case r == '\\' && quote == '"':
				escaped = true
			case r == quote:
				quote = 0
			}
			continue
		}
		switch r {
		case '"', '\'':
			quote = r
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	return append(parts, strings.TrimSpace(s[start:]))
}
