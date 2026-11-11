// Package transcript reads Claude Code session transcript JSONL: the live context size and the files a session wrote.
package transcript

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/edotau/claude-code/internal/textutil"
)

// EachLine calls fn per line of r (oversized joined, EOF partial kept); slice valid in fn only; returns non-EOF read errors.
func EachLine(r io.Reader, fn func([]byte)) error {
	_, err := textutil.ScanRawLines(r, false, fn)
	return err
}

// contentPart is one element of a message.content array; only {type:"text"} parts carry transcript text.
type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ContentText flattens message.content (bare string, or {type,text} parts) to its text parts joined; others skipped.
func ContentText(content json.RawMessage) string {
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	var parts []contentPart
	if err := json.Unmarshal(content, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
