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

const pemKeyLabel = "PRIVATE KEY"

var pemBegin, pemEnd, pemKeyBegin = PEMBegin, PEMEnd, newPEMKeyBegin(PEMBegin, pemKeyLabel)

func newPEMKeyBegin(begin, label string) *regexp.Regexp {
	return regexp.MustCompile(regexp.QuoteMeta(begin) + `[A-Z ]*` + regexp.QuoteMeta(label))
}

// SwapPEMMarkers sets the BEGIN prefix, END prefix and key label the PEM helpers match, so a test holds no key marker.
func SwapPEMMarkers(begin, end, label string) (restore func()) {
	ob, oe, ok := pemBegin, pemEnd, pemKeyBegin
	pemBegin, pemEnd, pemKeyBegin = begin, end, newPEMKeyBegin(begin, label)
	return func() { pemBegin, pemEnd, pemKeyBegin = ob, oe, ok }
}

// AfterOrphanPEM is s past the END line of a PEM block whose BEGIN a cut lost; open: the dropped front left one open.
func AfterOrphanPEM(s string, open bool) string {
	end := strings.Index(s, pemEnd)
	if !open && (end < 0 || strings.Contains(s[:end], pemBegin)) {
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
	if end := bytes.LastIndex(b[:min(len(b), drop+len(pemEnd)-1)], []byte(pemEnd)); begin != end {
		return begin > end // neither marker in drop: the state from earlier drops stands
	}
	return open
}
