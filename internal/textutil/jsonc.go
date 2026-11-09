package textutil

// StripJSONC removes // and /* */ comments and trailing commas so VS Code's own settings.json
// dialect (JSONC) parses as plain JSON. String contents are left untouched either pass.
// An unterminated /* comment returns data unchanged so the caller's JSON parser rejects it.
func StripJSONC(data []byte) []byte {
	stripped, ok := stripComments(data)
	if !ok {
		return data
	}
	return stripTrailingCommas(stripped)
}

// strState tracks whether the scan is inside a JSON string literal and just past a backslash.
type strState struct{ in, escaped bool }

// step consumes c while inside a string and reports whether c belonged to the string (it must be copied through).
func (st *strState) step(c byte) bool {
	if !st.in {
		return false
	}
	switch {
	case st.escaped:
		st.escaped = false
	case c == '\\':
		st.escaped = true
	case c == '"':
		st.in = false
	}
	return true
}

// stripComments removes comments; ok is false when a /* comment never closes.
func stripComments(data []byte) (out []byte, ok bool) {
	out = make([]byte, 0, len(data))
	var st strState
	for i := 0; i < len(data); i++ {
		c := data[i]
		if st.step(c) {
			out = append(out, c)
			continue
		}
		switch {
		case c == '"':
			st.in = true
			out = append(out, c)
		case hasPair(data, i, '/', '/'):
			i = skipLineComment(data, i)
			if i < len(data) {
				out = append(out, '\n')
			}
		case hasPair(data, i, '/', '*'):
			end, closed := skipBlockComment(data, i)
			if !closed {
				return nil, false
			}
			i = end
		default:
			out = append(out, c)
		}
	}
	return out, true
}

// hasPair reports data[i]==a and data[i+1]==b.
func hasPair(data []byte, i int, a, b byte) bool {
	return data[i] == a && i+1 < len(data) && data[i+1] == b
}

// skipLineComment returns the index of the newline ending the // comment at i, or len(data).
func skipLineComment(data []byte, i int) int {
	for i < len(data) && data[i] != '\n' {
		i++
	}
	return i
}

// skipBlockComment returns the index of the closing '/' of the /* comment at i; closed is false if none.
func skipBlockComment(data []byte, i int) (end int, closed bool) {
	for j := i + 2; j+1 < len(data); j++ {
		if data[j] == '*' && data[j+1] == '/' {
			return j + 1, true
		}
	}
	return len(data), false
}

func stripTrailingCommas(data []byte) []byte {
	out := make([]byte, 0, len(data))
	var st strState
	for i := range data {
		c := data[i]
		if st.step(c) {
			out = append(out, c)
			continue
		}
		if c == '"' {
			st.in = true
			out = append(out, c)
			continue
		}
		if c == ',' && closesAfterSpace(data, i+1) {
			continue // drop the trailing comma
		}
		out = append(out, c)
	}
	return out
}

// closesAfterSpace reports whether the first non-space byte at or after j closes an object or array.
func closesAfterSpace(data []byte, j int) bool {
	for j < len(data) && isJSONSpace(data[j]) {
		j++
	}
	return j < len(data) && (data[j] == '}' || data[j] == ']')
}

func isJSONSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }
