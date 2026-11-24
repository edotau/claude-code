package redact

import (
	"bytes"
	"regexp"
	"strings"
)

const (
	PEMBegin = "-----BEGIN "
	PEMEnd   = "-----END "
)

// AfterOrphanPEM is s past the END line of a PEM block whose BEGIN a cut lost; open: the dropped front left one open.
func AfterOrphanPEM(s string, open bool) string {
	end := strings.Index(s, PEMEnd)
	if !open && (end < 0 || strings.Contains(s[:end], PEMBegin)) {
		return s
	}
	if end < 0 { // the open block's body runs to the end of the window: none of it is shown
		return ""
	}
	if nl := strings.IndexByte(s[end:], '\n'); nl >= 0 {
		return s[end+nl:]
	}
	return ""
}

var pemKeyBegin = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY`)

// pemLook is how far past a cut a marker may reach and still count: the longest key BEGIN label.
const pemLook = 40

// PEMOpenAfterDrop is whether the first drop bytes of b end inside a private-key block; open is the state before.
func PEMOpenAfterDrop(b []byte, drop int, open bool) bool {
	seen := b[:min(len(b), drop+pemLook)] // a marker split by the cut still counts
	begin := -1
	for _, m := range pemKeyBegin.FindAllIndex(seen, -1) {
		if m[0] < drop { // a marker starting in the window is the window's, not the dropped front's
			begin = m[0]
		}
	}
	if end := bytes.LastIndex(b[:min(len(b), drop+len(PEMEnd)-1)], []byte(PEMEnd)); begin != end {
		return begin > end // neither marker in drop: the state from earlier drops stands
	}
	return open
}
