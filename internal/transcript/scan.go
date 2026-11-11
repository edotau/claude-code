// Package transcript reads Claude Code session transcript JSONL: the live context size and the files a session wrote.
package transcript

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"regexp"
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

const dispatchIDExpr = `\d{8}T\d{6}\.\d{3}-[A-Za-z0-9._-]+`

// dispatchIDPattern matches the four shapes `ask` prints, raw or JSON-escaped inside a tool_result.
var dispatchIDPattern = regexp.MustCompile(
	`ask: dispatch (` + dispatchIDExpr + `)|job (` + dispatchIDExpr + `) started|\\*"(?:id|dispatch_id)\\*":\s*\\*"(` + dispatchIDExpr + `)\\*"`)

var toolResultMarker = []byte(`"tool_result"`)

// DispatchIDs lists the dispatch ids a transcript's tool results printed, deduped, in first-seen order; nil if unreadable.
func DispatchIDs(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var ids []string
	seen := map[string]bool{}
	_ = EachLine(f, func(line []byte) { ids = AppendDispatchIDs(ids, seen, line) })
	return ids
}

// AppendDispatchIDs adds the dispatch ids one transcript line's tool result printed that seen lacks, marking them seen.
func AppendDispatchIDs(ids []string, seen map[string]bool, line []byte) []string {
	if !bytes.Contains(line, toolResultMarker) {
		return ids
	}
	for _, m := range dispatchIDPattern.FindAllSubmatch(line, -1) {
		id := string(m[1]) + string(m[2]) + string(m[3])
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}
