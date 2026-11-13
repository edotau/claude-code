package shell

import "regexp"

// fdDup is a `>&N`, `>&N-` or `>&-` operand ending at whitespace, `)` or the segment's end.
var fdDup = regexp.MustCompile(`^>&(-|[0-9]+-?)([\s)]|$)`)

// FileRedirect reports an unquoted output redirection that may write a file; `2>&1`, `>&2`, `2>&-` are not one.
func FileRedirect(seg string) bool {
	at := QuoteAt(seg)
	for i := 0; i < len(seg); i++ {
		if at[i] != 0 || seg[i] != '>' {
			continue
		}
		if i > 0 && (seg[i-1] == '&' || seg[i-1] == '<' || seg[i-1] == '>') || !fdDup.MatchString(seg[i:]) {
			return true
		}
	}
	return false
}
