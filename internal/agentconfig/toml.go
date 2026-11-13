package agentconfig

import (
	"encoding/json"
	"errors"
	"iter"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/textutil"
)

// TOMLQuote renders s as a TOML basic string (a JSON string is one) for a codex -c override.
func TOMLQuote(s string) string {
	b, _ := json.Marshal(s)                                // a JSON string is a valid TOML basic string
	return strings.ReplaceAll(string(b), "\x7f", `\u007f`) // TOML forbids a raw DEL; JSON leaves it
}

// TOMLQuoteRaw is TOMLQuote without JSON's \u003c, \u003e and \u0026 escapes for <, > and &.
func TOMLQuoteRaw(s string) string {
	b, _ := textutil.MarshalNoEscape(s) // a string always encodes
	return strings.ReplaceAll(string(b), "\x7f", `\u007f`)
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

// InsertTopLevelTOML puts section before raw's first top-level table header, dropping top-level keys it re-sets.
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

// topLevelKeysOf returns the bare key names section assigns before its own first table header.
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
			if key, ok := assignedKey(trimmed); ok {
				keys = append(keys, key)
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

// dropTopLevelKeys removes top-level assignments of keys, continuation lines included; table keys stay.
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
	key, ok := assignedKey(strings.TrimSpace(line))
	return ok && slices.Contains(keys, key)
}

// assignedKey is the key of a "key = value" line, split at the first '=' outside quotes; a dotted key keeps its dots.
func assignedKey(line string) (string, bool) {
	dotted := false
	for i := 0; i < len(line); {
		switch c := line[i]; c {
		case '"', '\'':
			i = skipQuoted(line, i+1, c)
		case '=':
			if dotted {
				return dropUnquotedSpace(line[:i]), true
			}
			return TOMLKeyName(line[:i]), true
		case '.':
			dotted = true
			i++
		default:
			i++
		}
	}
	return "", false
}

// dropUnquotedSpace removes the blanks around a dotted key's parts, keeping quoted parts ("a b") byte-for-byte.
func dropUnquotedSpace(key string) string {
	var b strings.Builder
	for i := 0; i < len(key); {
		switch c := key[i]; c {
		case '"', '\'':
			end := skipQuoted(key, i+1, c)
			b.WriteString(key[i:end])
			i = end
		case ' ', '\t':
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// FirstTopLevelHeaderIndex finds the first top-level table header line, outside strings and arrays; -1 for none.
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

// TOMLLineState tracks multiline-string state and bracket depth, so an array continuation is no table header.
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
	end, after := tripleEnd(value[3:], quote[0])
	if end < 0 {
		return "", "", ErrOpenString
	}
	body := strings.TrimPrefix(strings.TrimPrefix(value[3:3+end], "\r"), "\n")
	rest = value[3+after:]
	if quote[0] == '"' {
		body, err = multilineBasic(body)
	}
	return body, rest, err
}

// tripleEnd finds the closing triple quote in s: the last three of a 3-5 quote run (two may end the body).
func tripleEnd(s string, quote byte) (end, after int) {
	for i := 0; i < len(s); {
		switch {
		case quote == '"' && s[i] == '\\':
			i += 2
		case s[i] == quote:
			run := len(s[i:]) - len(strings.TrimLeft(s[i:], string(quote)))
			if run >= 3 {
				run = min(run, 5)
				return i + run - 3, i + run
			}
			i += run
		default:
			i++
		}
	}
	return -1, -1
}

// multilineBasic unescapes a """ body: a line-ending backslash joins lines, other newlines become \n.
func multilineBasic(body string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		switch c := body[i]; {
		case c == '\\' && i+1 < len(body):
			if j := lineEndingBackslash(body, i); j > 0 {
				i = j - 1
				continue
			}
			b.WriteString(body[i : i+2])
			i++
		case c == '"':
			b.WriteString(`\"`)
		case c == '\n':
			b.WriteString(`\n`)
		case c != '\r':
			b.WriteByte(c)
		}
	}
	quoted := `"` + b.String() + `"`
	if err := checkEscapes(quoted); err != nil {
		return "", err
	}
	return strconv.Unquote(quoted)
}

// checkEscapes rejects the escapes Go's strconv.Unquote accepts but TOML does not (\a, \v, \x, octal, \').
func checkEscapes(quoted string) error {
	for i := 0; i < len(quoted); i++ {
		if quoted[i] != '\\' {
			continue
		}
		i++
		if i >= len(quoted) || !strings.ContainsRune(`btnfr"\\uU`, rune(quoted[i])) {
			return errors.New("invalid TOML string escape")
		}
	}
	return nil
}

// lineEndingBackslash returns the index past the whitespace a backslash at i trims, or 0 when no newline follows it.
func lineEndingBackslash(body string, i int) int {
	rest := strings.TrimLeft(body[i+1:], " \t")
	if !strings.HasPrefix(rest, "\n") && !strings.HasPrefix(rest, "\r\n") {
		return 0
	}
	return len(body) - len(strings.TrimLeft(rest, " \t\r\n"))
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
	if err = checkEscapes(quoted); err != nil {
		return "", "", err
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
	for i := 1; i < len(line); {
		switch c := line[i]; c {
		case '"', '\'':
			i = skipQuoted(line, i+1, c)
		case ']':
			return i
		default:
			i++
		}
	}
	return -1
}

// SplitInlineTable splits top-level TOML inline-table commas, ignoring quoted strings and nested tables or arrays.
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
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\'':
			i = skipQuoted(s, i+1, c) - 1
		case '{', '[':
			depth++
		case '}', ']':
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
