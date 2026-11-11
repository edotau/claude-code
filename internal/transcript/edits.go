package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/textutil"
)

var editTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}
var toolUseKey = []byte(`"tool_use"`)
var commitKey = []byte("commit") // cheap prefilter; the real check is gitCommitCommand (segment/token aware)
var bashNameKey = []byte(`"name":"Bash"`)

// editNameKeys are the literal `"name":"<Tool>"` byte patterns for editTools — cheap enough to rule a
// line out before paying for json.Unmarshal, since every other tool_use (Read/Grep/Bash…) fails all four.
var editNameKeys = [][]byte{
	[]byte(`"name":"Edit"`), []byte(`"name":"Write"`), []byte(`"name":"MultiEdit"`), []byte(`"name":"NotebookEdit"`),
}

func hasEditToolName(line []byte) bool {
	for _, k := range editNameKeys {
		if bytes.Contains(line, k) {
			return true
		}
	}
	return false
}

// CommitEditWeight is how many edits a Bash `git commit` call counts for: one commit alone is
// substantive work; memory.updateMinEdits is defined from this so the two stay in lockstep.
const CommitEditWeight = 3

// EditedFiles returns up to limit unique paths the transcript's Edit/Write tool calls touched, sorted.
func EditedFiles(path string, limit int) ([]string, error) {
	seen := map[string]bool{}
	err := eachEdit(path, func(p string) {
		if p != "" {
			seen[p] = true
		}
	})
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, err
}

// CountEditsFrom counts complete lines from offset and returns the position after the last newline.
func CountEditsFrom(path string, offset int64) (edits int, next int64) {
	edits, _, next = CountWorkFrom(path, offset)
	return edits, next
}

// CountWorkFrom counts complete lines from offset, returning edits, user turns, and next offset.
func CountWorkFrom(path string, offset int64) (edits, turns int, next int64) {
	offset = max(offset, 0)
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, offset
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		return 0, 0, offset
	}
	if fi.Size() < offset {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, 0, offset
	}
	read, _ := textutil.ScanRawLines(f, true, func(line []byte) {
		edits += countLineEdits(line)
		if isUserTurnLine(line) {
			turns++
		}
	})
	return edits, turns, offset + read
}

var userTypeKey = []byte(`"type":"user"`)

// toolResultKey only matches a real JSON value: inside prompt text the closing quote is escaped.
var toolResultKey = []byte(`"tool_result"`)

// isUserTurnLine reports whether line is a typed user prompt: not meta, no tool_result, not a slash-command echo.
func isUserTurnLine(line []byte) bool {
	if !bytes.Contains(line, userTypeKey) || bytes.Contains(line, toolResultKey) {
		return false
	}
	var rec struct {
		Type    string `json:"type"`
		IsMeta  bool   `json:"isMeta"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Type != "user" || rec.IsMeta {
		return false
	}
	text := strings.TrimSpace(ContentText(rec.Message.Content))
	return text != "" && !strings.HasPrefix(text, "<command-") && !strings.HasPrefix(text, "<local-command")
}

// mayCountEdits is the cheap byte pre-filter: a tool_use line naming an editor tool or a Bash commit.
func mayCountEdits(line []byte) bool {
	if !bytes.Contains(line, toolUseKey) {
		return false
	}
	return hasEditToolName(line) || bytes.Contains(line, bashNameKey) && bytes.Contains(line, commitKey)
}

// countLineEdits decodes an assistant tool_use line at most once, counting editor calls and weighted git commits.
func countLineEdits(line []byte) (n int) {
	defer func() { _ = recover() }() // malformed command text must not crash the hook; keep edits counted so far
	if !mayCountEdits(line) {
		return 0
	}
	var rec struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type  string `json:"type"`
				Name  string `json:"name"`
				Input struct {
					Command string `json:"command"`
				} `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Type != "assistant" {
		return 0
	}
	for _, c := range rec.Message.Content {
		if c.Type != "tool_use" {
			continue
		}
		switch {
		case editTools[c.Name]:
			n++
		case c.Name == "Bash" && gitCommitCommand(c.Input.Command):
			n += CommitEditWeight
		}
	}
	return n
}

// eachEdit calls fn with the target path of each editor tool_use in an assistant record.
func eachEdit(path string, fn func(filePath string)) error {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // no transcript yet: no edits
	}
	if err != nil {
		return err
	}
	defer f.Close()
	return EachLine(f, func(line []byte) {
		eachEditLine(line, fn)
	})
}

func eachEditLine(line []byte, fn func(filePath string)) {
	if !bytes.Contains(line, toolUseKey) || !hasEditToolName(line) {
		return
	}
	var rec struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type  string `json:"type"`
				Name  string `json:"name"`
				Input struct {
					FilePath     string `json:"file_path"`
					NotebookPath string `json:"notebook_path"`
				} `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Type != "assistant" {
		return
	}
	for _, c := range rec.Message.Content {
		if c.Type == "tool_use" && editTools[c.Name] {
			fn(c.Input.FilePath + c.Input.NotebookPath)
		}
	}
}
