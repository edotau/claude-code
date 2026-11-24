package safety

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/shell"
)

var shellNames = sync.OnceValue(func() map[string]bool { return shell.GuardSet("shell_names") })

func isAssignment(w string) bool {
	name, _, ok := strings.Cut(w, "=")
	return ok && name != "" && !strings.ContainsAny(name, "-/ ")
}

// shellFeeds is cmd with each decodable printf/echo-into-a-shell pipeline replaced by `:`, and the texts it runs.
func shellFeeds(cmd string) (string, []string) {
	if !strings.Contains(cmd, "|") {
		return cmd, nil
	}
	segs, seps := shell.SegmentsSep(cmd)
	var b strings.Builder
	var texts []string
	for i := 0; i < len(segs); i++ {
		if end, text, ok := shellFeed(segs, seps, i); ok {
			texts = append(texts, text...)
			b.WriteString(" :")
			b.WriteString(seps[end])
			i = end
			continue
		}
		b.WriteString(segs[i])
		b.WriteString(seps[i])
	}
	return b.String(), texts
}

// shellFeed decodes a printf/echo at segs[i] piped, through filters only, into a stdin shell ending at segs[end].
func shellFeed(segs, seps []string, i int) (end int, texts []string, ok bool) {
	if strings.ContainsAny(segs[i], "$`<>") { // an expansion or redirect cannot be decoded from the text
		return 0, nil, false
	}
	texts, ok = printedText(shell.Tokens(segs[i]))
	for k := i; ok && k+1 < len(segs) && pipedOn(seps[k]); k++ {
		rest, _ := shell.Unwrap(shell.Tokens(segs[k+1]), "")
		switch {
		case stdinShell(rest):
			return k + 1, texts, true
		case !feedFilter(rest):
			return 0, nil, false
		}
	}
	return 0, nil, false
}

func pipedOn(sep string) bool { return sep == "|" || sep == "|&" }

// feedFilter reports a pipe stage that only passes text on: a pure filter or cat, with no redirect.
func feedFilter(rest []string) bool {
	return len(rest) > 0 && (pipeFilters()[rest[0]] || rest[0] == "cat") && inertEcho(rest)
}

// stdinShell reports sh/bash/zsh/dash/ksh given only flags and no -c string: it runs the commands on its stdin.
func stdinShell(toks []string) bool {
	if len(toks) == 0 || !shellNames()[filepath.Base(toks[0])] {
		return false
	}
	return !slices.ContainsFunc(toks[1:], func(t string) bool {
		return !strings.HasPrefix(t, "-") || !strings.HasPrefix(t, "--") && strings.Contains(t, "c")
	})
}

// printedText is what a printf or echo prints; echo's text is checked as bash and as zsh/dash print it (escapes).
func printedText(toks []string) ([]string, bool) {
	if len(toks) < 2 {
		return nil, false
	}
	words := toks[1:]
	switch toks[0] {
	case "printf":
		if strings.HasPrefix(words[0], "-") { // -v assigns instead of printing; -- is rare
			return nil, false
		}
		return []string{printfOutput(words[0], words[1:])}, true
	case "echo":
		for len(words) > 0 && echoFlag(words[0]) {
			words = words[1:]
		}
		raw := strings.Join(words, " ")
		return []string{raw, decodeEscapes(raw)}, true
	}
	return nil, false
}

// echoFlag reports an echo option word such as -n, -e or -neE.
func echoFlag(w string) bool {
	return len(w) > 1 && w[0] == '-' && strings.Trim(w[1:], "neE") == ""
}

// printfOutput applies a printf format to args as the shell does, reusing the format while args remain.
func printfOutput(format string, args []string) string {
	var out strings.Builder
	for {
		used := printfPass(&out, format, args)
		if args = args[used:]; used == 0 || len(args) == 0 {
			return decodeEscapes(out.String())
		}
	}
}

// printfPass writes one pass of format, `%b` arguments decoded, and returns how many args it consumed.
func printfPass(out *strings.Builder, format string, args []string) int {
	used := 0
	for i := 0; i < len(format); i++ {
		if format[i] != '%' || i+1 >= len(format) {
			out.WriteByte(format[i])
			continue
		}
		j := i + 1
		for j < len(format) && strings.IndexByte("-+ #0123456789.", format[j]) >= 0 {
			j++
		}
		if j >= len(format) || format[j] == '%' {
			out.WriteByte('%')
			i = j
			continue
		}
		arg := ""
		if used < len(args) {
			arg, used = args[used], used+1
		}
		if format[j] == 'b' {
			arg = decodeEscapes(arg)
		}
		out.WriteString(strings.ReplaceAll(arg, `\`, `\\`)) // literal: survives the final escape pass
		i = j
	}
	return used
}

// decodeEscapes applies printf/echo -e backslash escapes: \n \t \\, octal \0NNN or \NNN, hex \xHH; \c ends output.
func decodeEscapes(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			out.WriteByte(s[i])
			continue
		}
		i++
		switch c := s[i]; {
		case c == 'c':
			return out.String()
		case strings.IndexByte("ntrvfab", c) >= 0:
			out.WriteByte("\n\t\r\v\f\a\b"[strings.IndexByte("ntrvfab", c)])
		case c == 'x' || c >= '0' && c <= '7':
			n, width := escapeValue(s[i:])
			out.WriteByte(n)
			i += width - 1
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

// escapeValue reads a \xHH (up to 2 hex digits) or \0NNN / \NNN (up to 3 octal digits) escape after its backslash.
func escapeValue(s string) (byte, int) {
	base, digits, from, most := 8, "01234567", 0, 3
	switch {
	case s[0] == 'x':
		base, digits, from, most = 16, "0123456789abcdefABCDEF", 1, 2
	case s[0] == '0' && len(s) > 1 && strings.IndexByte(digits, s[1]) >= 0:
		from = 1
	}
	n, k := 0, from
	for ; k < len(s) && k-from < most && strings.IndexByte(digits, s[k]) >= 0; k++ {
		v, _ := strconv.ParseInt(s[k:k+1], 16, 0)
		n = n*base + int(v)
	}
	if k == from { // `\x` with no hex digit prints as written
		return 'x', 1
	}
	return byte(n), k
}
