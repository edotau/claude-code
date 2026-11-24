package redact

import (
	"bytes"
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

// PEMOpenAfterDrop is whether the first drop bytes of b end inside a PEM block; open is the state before the drop.
func PEMOpenAfterDrop(b []byte, drop int, open bool) bool {
	seen := b[:drop+len(PEMBegin)-1] // a marker split by the cut still counts
	if bi, e := bytes.LastIndex(seen, []byte(PEMBegin)), bytes.LastIndex(seen, []byte(PEMEnd)); bi != e {
		return bi > e // neither marker in drop: the state from earlier drops stands
	}
	return open
}
