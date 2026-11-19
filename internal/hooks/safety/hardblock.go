package safety

import (
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/shell"
)

// rmRecursive matches `rm` with any recursive flag spelling (-r, -Rf, -r -f, --recursive) and trailing operands.
const rmRecursive = `(^|[^[:alnum:]_])rm\s+(-\S+\s+)*(-[a-zA-Z]*[rR][a-zA-Z]*|--recursive)\s+(\S+\s+)*`

// hardBlocks are command shapes that are almost never intentional; each is gated on a literal every match holds.
var hardBlocks = []*core.Gated{
	core.Gate(rmRecursive+`["']?/(\*|\s|$|[)\x60"'])`, "rm"),
	core.Gate(rmRecursive+`["']?/(etc|usr|var|bin|sbin|lib|lib64|boot|opt|root|System|Users|home|Applications|Library|Volumes|private)(/?\s|/?$|/?[)\x60"'])`, "rm"),
	core.Gate(rmRecursive+`["']?(\$HOME|\$\{HOME\}|~)/?(\*|\s|$|[)\x60"'])`, "rm"),
	core.Gate(`mkfs\.`, "mkfs."),
	core.Gate(`dd\s+(.*\s)?of=/dev/r?(sd|disk|nvme|hd)`, "of=/dev/"),
	core.Gate(`chmod\s+-R\s+0?777\s+/(\s|$)`, "chmod"),
	core.Gate(`python[0-9.]*\s+.*-c\s+.*exec\(.*base64`, "base64"),
}

// hardBlockVerdict blocks a hard shape outside inert text; one only in inert text is data.
func hardBlockVerdict(in *core.Input, cmd, scan string) (core.Decision, string, string) {
	if strings.Contains(cmd, "<<") { // heredoc bodies stay in the scan
		scan = cmd
	}
	if pattern, ok := firstHardBlock(blankHardData(scan)); ok {
		return core.Block, core.SafetyMessage(in, core.Block, "destructive command", pattern, terminalHint), pattern
	}
	if pattern, ok := feedHardBlock(cmd); ok { // `printf 'rm -rf /\n' | sh` runs what printf prints
		return core.Block, core.SafetyMessage(in, core.Block, "destructive command", pattern, terminalHint), pattern
	}
	for _, s := range shell.RunScripts(cmd, in.CWD) { // `printf 'rm -rf /\n' > f && sh f` runs the file
		if pattern, ok := firstHardBlock(s.Text); ok {
			return core.Block, core.SafetyMessage(in, core.Block, "destructive command", pattern, terminalHint), pattern
		}
	}
	return core.Allow, "", ""
}

// firstHardBlock is the first hard-block pattern any shell segment of text matches, as written or as the shell reads it
func firstHardBlock(text string) (string, bool) {
	for _, seg := range shell.Segments(text) {
		read := shellRead(seg)
		for _, re := range hardBlocks {
			if re.MatchString(seg) || re.MatchString(read) {
				return re.String(), true
			}
		}
		if inner, ok := shell.NestedCommand(shell.Tokens(seg)); ok { // `sh -c "'rm' -rf /"` runs its string
			if pattern, hit := firstHardBlock(blankHardData(inner)); hit {
				return pattern, true
			}
		}
	}
	return "", false
}

// xargsHardBlock is the hard-block pattern an `echo/printf TEXT | xargs rm …` runs once xargs appends TEXT's words.
func xargsHardBlock(cmd string) (string, bool) {
	if !strings.Contains(cmd, "xargs") {
		return "", false
	}
	segs, seps := shell.SegmentsSep(cmd)
	for k := 0; k+1 < len(segs); k++ {
		texts, ok := printedText(shell.Tokens(segs[k]))
		rest, _ := shell.Unwrap(shell.DropRtk(shell.Tokens(segs[k+1])), "")
		if !ok || !pipedOn(seps[k]) || len(rest) == 0 || filepath.Base(rest[0]) != "xargs" {
			continue
		}
		if run := xargsRun(rest); run >= 0 && filepath.Base(rest[run]) == "rm" {
			if pattern, hit := appendedHardBlock(strings.Join(rest[run:], " "), texts); hit {
				return pattern, true
			}
		}
	}
	return "", false
}

// xargsRun is the index in rest (`xargs [opts] cmd …`) of the command xargs runs, or -1.
func xargsRun(rest []string) int {
	for i := 1; i < len(rest); i++ {
		if !strings.HasPrefix(rest[i], "-") {
			return i
		}
		if len(rest[i]) == 2 && strings.Contains("IEnLPsad", rest[i][1:]) && i+1 < len(rest) {
			i++ // a separate-argument option such as `-n 1`
		}
	}
	return -1
}

// appendedHardBlock is the hard-block pattern run, with one word of texts appended, matches.
func appendedHardBlock(run string, texts []string) (string, bool) {
	for _, text := range texts {
		for word := range strings.FieldsSeq(text) {
			if pattern, hit := firstHardBlock(run + " " + word); hit {
				return pattern, true
			}
		}
	}
	return "", false
}

// feedHardBlock is the first hard-block pattern the text a printf/echo pipes into a shell matches, decoded.
func feedHardBlock(cmd string) (string, bool) {
	_, texts := shellFeeds(cmd)
	for _, text := range texts {
		if pattern, ok := firstHardBlock(text); ok {
			return pattern, true
		}
	}
	return xargsHardBlock(cmd)
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
	for k := i; ok && (seps[k] == "|" || seps[k] == "|&") && k+1 < len(segs); k++ {
		rest, _ := shell.Unwrap(shell.Tokens(segs[k+1]), "")
		switch {
		case stdinShell(rest):
			return k + 1, texts, true
		case len(rest) == 0 || !pipeFilters()[rest[0]] && rest[0] != "cat" || !inertEcho(rest):
			return 0, nil, false
		}
	}
	return 0, nil, false
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

// shellRead is seg with its words unquoted and unescaped and each path operand cleaned: `"rm" -rf //` reads `rm -rf /`.
func shellRead(seg string) string {
	toks := shell.Tokens(seg)
	for i, t := range toks {
		toks[i] = cleanOperand(t)
	}
	return strings.Join(toks, " ")
}

// cleanOperand collapses `//`, `.` and `..` in an absolute or home-relative word; a home path climbing out reads `/`
func cleanOperand(word string) string {
	for _, home := range []string{"${HOME}", "$HOME", "~"} {
		rest, ok := strings.CutPrefix(word, home)
		if !ok || (rest != "" && rest[0] != '/') {
			continue
		}
		switch up := path.Clean("h" + rest); {
		case up == "h":
			return "$HOME"
		case strings.HasPrefix(up, "h/"):
			return "$HOME" + up[1:]
		default:
			return "/"
		}
	}
	if strings.HasPrefix(word, "/") {
		return path.Clean(word)
	}
	return word
}
