package shell

import "strings"

// isFdDup reports a `>&N`, `>&N-` or `>&-` operand at s[0:] ending at whitespace, `)` or end.
func isFdDup(s string) bool {
	if len(s) < 3 || s[0] != '>' || s[1] != '&' {
		return false
	}
	i := 2
	if s[i] == '-' {
		i++
	} else {
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
		if i < len(s) && s[i] == '-' {
			i++
		}
	}
	return i == len(s) || s[i] == ')' || isSpaceByte(s[i])
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

// FileRedirect reports an unquoted output redirection that may write a file; `2>&1`, `>&2`, `>/dev/null` are not one.
func FileRedirect(seg string) bool {
	if !strings.Contains(seg, ">") { // only a `>` byte can return true below
		return false
	}
	at := redirectQuotes(seg)
	for i := 0; i < len(seg); i++ {
		if at[i] != 0 || seg[i] != '>' {
			continue
		}
		if (i > 0 && (seg[i-1] == '&' || seg[i-1] == '<' || seg[i-1] == '>') || !isFdDup(seg[i:])) && !toDevNull(seg, at, i) {
			return true
		}
	}
	return false
}

// toDevNull reports that the redirect operator whose `>` is at seg[i] has exactly `/dev/null` as its operand.
func toDevNull(seg string, at []byte, i int) bool {
	from := i
	for from < len(seg) && strings.IndexByte(">|&", seg[from]) >= 0 {
		from++
	}
	for from < len(seg) && (seg[from] == ' ' || seg[from] == '\t') {
		from++
	}
	return seg[from:operandEnd(seg, at, from)] == "/dev/null"
}

// redirectOps are the operators Redirects reads, longest first; out marks the ones that open a file for writing.
var redirectOps = []struct {
	op  string
	out bool
}{{"&>>", true}, {"<<<", false}, {"<<-", false}, {"&>", true}, {">>", true}, {">|", true}, {">&", true},
	{"<>", true}, {"<<", false}, {"<&", false}, {">", true}, {"<", false}}

// isFdNumber reports a `>&`/`<&` operand that names a descriptor (`-`, `N`, `N-`), not a file.
func isFdNumber(s string) bool {
	if s == "-" {
		return true
	}
	if len(s) == 0 {
		return false
	}
	if s[len(s)-1] == '-' {
		s = s[:len(s)-1]
	}
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Redirects splits seg into its words minus unquoted redirections, and the files output ones write (`a>f`, `2>f`).
func Redirects(seg string) (words, targets []string) {
	if !strings.ContainsAny(seg, "<>&") {
		return Tokens(seg), nil
	}
	at := redirectQuotes(seg)
	buf := make([]byte, 0, len(seg))
	consumed := 0 // end of the last redirection's operand: digits before it are that operand, not an fd
	for i := 0; i < len(seg); i++ {
		start, op, out, ok := redirectAt(seg, at, i)
		if !ok {
			buf = append(buf, seg[i])
			continue
		}
		start = max(start, consumed)
		from := i + len(op)
		for from < len(seg) && (seg[from] == ' ' || seg[from] == '\t') {
			from++
		}
		end := operandEnd(seg, at, from)
		word := Tokens(seg[from:end])
		if out && len(word) == 1 && (op != ">&" || !isFdNumber(word[0])) {
			targets = append(targets, word[0])
		}
		buf = append(buf[:len(buf)-(i-start)], ' ')
		consumed, i = end, end-1
	}
	return Tokens(string(buf)), targets
}

func redirectsTokens(seg string, tokens []string) (words, targets []string) {
	if !strings.ContainsAny(seg, "<>&") {
		return tokens, nil
	}
	return Redirects(seg)
}

// redirectQuotes is QuoteAt with each `${…}`/`$((…))` byte marked quoted: `$((3>2))` redirects nothing.
func redirectQuotes(seg string) []byte {
	at := QuoteAt(seg)
	if strings.Contains(seg, "${") || strings.Contains(seg, "$((") {
		for i, in := range exprMask(seg) {
			if in {
				at[i] = '\''
			}
		}
	}
	return at
}

// redirectAt reports an unquoted redirection operator at seg[i] (not `<(`/`>(`), and where it starts with its fd.
func redirectAt(seg string, at []byte, i int) (start int, op string, out, ok bool) {
	if at[i] != 0 || seg[i] != '<' && seg[i] != '>' && seg[i] != '&' {
		return 0, "", false, false
	}
	for _, r := range redirectOps {
		if strings.HasPrefix(seg[i:], r.op) {
			op, out = r.op, r.out
			break
		}
	}
	if op == "" || strings.HasPrefix(seg[i+len(op):], "(") {
		return 0, "", false, false
	}
	start = i
	for start > 0 && seg[start-1] >= '0' && seg[start-1] <= '9' && at[start-1] == 0 {
		start--
	}
	if start < i && start > 0 && strings.IndexByte(" \t\n;&|(", seg[start-1]) < 0 {
		start = i // digits glued to a word (`a2>f`) are that word's, not an fd
	}
	return start, op, out, true
}

// operandEnd is the end of the word starting at seg[from]: the first unquoted blank, separator or redirection byte.
func operandEnd(seg string, at []byte, from int) int {
	for j := from; j < len(seg); j++ {
		if at[j] != 0 {
			continue
		}
		if _, end, ok := substitutionAt(seg, j, 0); ok && (seg[j] == '$' || seg[j] == '`') {
			j = end // `> $(mktemp)` is one word
			continue
		}
		if strings.IndexByte(" \t\n;&|<>()", seg[j]) >= 0 {
			return j
		}
	}
	return len(seg)
}
