package source

// pyMaskState is the byte state machine used by maskPythonSource.
type pyMaskState int

const (
	pyNormal pyMaskState = iota
	pyComment
	pySingle
	pyDouble
	pyTripleSingle
	pyTripleDouble
)

// maskPythonSource blanks string/comment interiors (one-pass byte scan) so keyword counting downstream
// never sees a keyword that lives inside a string literal or a # comment. Structure (indent, newlines,
// parens/brackets) is preserved so line numbers and continuation tracking stay accurate.
func maskPythonSource(src string) string {
	b := []byte(src)
	out := make([]byte, len(b))
	copy(out, b)
	state := pyNormal
	for i := 0; i < len(b); {
		switch state {
		case pyNormal:
			i, state = pyMaskNormal(b, out, i, &state)
		case pyComment:
			if b[i] == '\n' {
				state = pyNormal
			} else {
				maskByte(out, i)
			}
			i++
		default:
			i = pyMaskInString(b, out, i, &state)
		}
	}
	return string(out)
}

// pyMaskNormal handles one byte outside any string/comment; may switch state and/or skip ahead.
func pyMaskNormal(b, out []byte, i int, state *pyMaskState) (int, pyMaskState) {
	c := b[i]
	switch {
	case c == '#':
		*state = pyComment
		out[i] = ' '
		return i + 1, *state
	case (c == '\'' || c == '"') && tripleAt(b, i, c):
		*state = tripleState(c)
		blankN(out, i, 3)
		return i + 3, *state
	case c == '\'':
		*state = pySingle
	case c == '"':
		*state = pyDouble
	}
	return i + 1, *state
}

// pyMaskInString handles one byte inside a (possibly triple) quoted string, honoring backslash escapes.
func pyMaskInString(b, out []byte, i int, state *pyMaskState) int {
	c := b[i]
	triple := *state == pyTripleSingle || *state == pyTripleDouble
	quote := byte('\'')
	if *state == pyDouble || *state == pyTripleDouble {
		quote = '"'
	}
	if c == '\\' {
		maskByte(out, i)
		if i+1 < len(b) {
			maskByte(out, i+1)
		}
		return i + 2
	}
	if triple && tripleAt(b, i, quote) {
		*state = pyNormal
		blankN(out, i, 3)
		return i + 3
	}
	if !triple && (c == quote || c == '\n') {
		*state = pyNormal
		return i + 1
	}
	maskByte(out, i)
	return i + 1
}

// maskByte blanks one byte, but a newline is structure (line numbers), never string/comment content.
func maskByte(out []byte, i int) {
	if out[i] != '\n' {
		out[i] = ' '
	}
}

func tripleState(q byte) pyMaskState {
	if q == '\'' {
		return pyTripleSingle
	}
	return pyTripleDouble
}

// tripleAt reports whether b[i:i+3] is q,q,q (a triple-quote delimiter).
func tripleAt(b []byte, i int, q byte) bool {
	return i+2 < len(b) && b[i] == q && b[i+1] == q && b[i+2] == q
}

// blankN overwrites n bytes starting at i with spaces, preserving any embedded newline.
func blankN(out []byte, i, n int) {
	for k := 0; k < n && i+k < len(out); k++ {
		if out[i+k] != '\n' {
			out[i+k] = ' '
		}
	}
}
