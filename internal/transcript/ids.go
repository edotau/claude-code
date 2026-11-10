package transcript

import (
	"bytes"
	"os"
	"regexp"
)

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
	_ = EachLine(f, func(line []byte) {
		if !bytes.Contains(line, toolResultMarker) {
			return
		}
		for _, m := range dispatchIDPattern.FindAllSubmatch(line, -1) {
			id := string(m[1]) + string(m[2]) + string(m[3])
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	})
	return ids
}
