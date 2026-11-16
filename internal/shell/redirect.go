package shell

import (
	"regexp"
	"strings"
	"sync"
)

// fdDup is a `>&N`, `>&N-` or `>&-` operand ending at whitespace, `)` or the segment's end.
var fdDup = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^>&(-|[0-9]+-?)([\s)]|$)`) })

// FileRedirect reports an unquoted output redirection that may write a file; `2>&1`, `>&2`, `2>&-` are not one.
func FileRedirect(seg string) bool {
	at := redirectQuotes(seg)
	for i := 0; i < len(seg); i++ {
		if at[i] != 0 || seg[i] != '>' {
			continue
		}
		if i > 0 && (seg[i-1] == '&' || seg[i-1] == '<' || seg[i-1] == '>') || !fdDup().MatchString(seg[i:]) {
			return true
		}
	}
	return false
}

// redirectOps are the operators Redirects reads, longest first; out marks the ones that open a file for writing.
var redirectOps = []struct {
	op  string
	out bool
}{{"&>>", true}, {"<<<", false}, {"<<-", false}, {"&>", true}, {">>", true}, {">|", true}, {">&", true},
	{"<>", true}, {"<<", false}, {"<&", false}, {">", true}, {"<", false}}

// fdNumber is a `>&`/`<&` operand that names a descriptor, not a file.
var fdNumber = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^(-|[0-9]+-?)$`) })

// Redirects splits seg into its words minus unquoted redirections, and the files output ones write (`a>f`, `2>f`).
func Redirects(seg string) (words, targets []string) {
	at := redirectQuotes(seg)
	var b strings.Builder
	for i := 0; i < len(seg); i++ {
		start, op, out, ok := redirectAt(seg, at, i)
		if !ok {
			b.WriteByte(seg[i])
			continue
		}
		from := i + len(op)
		for from < len(seg) && (seg[from] == ' ' || seg[from] == '\t') {
			from++
		}
		end := operandEnd(seg, at, from)
		word := Tokens(seg[from:end])
		if out && len(word) == 1 && (op != ">&" || !fdNumber().MatchString(word[0])) {
			targets = append(targets, word[0])
		}
		trimmed := b.String()[:b.Len()-(i-start)] // drop the fd digits or `&` already written
		b.Reset()
		b.WriteString(trimmed + " ")
		i = end - 1
	}
	return Tokens(b.String()), targets
}

// redirectQuotes is QuoteAt with each `${…}`/`$((…))` byte marked quoted: `$((3>2))` redirects nothing.
func redirectQuotes(seg string) []byte {
	at := QuoteAt(seg)
	for i, in := range exprMask(seg) {
		if in {
			at[i] = '\''
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
