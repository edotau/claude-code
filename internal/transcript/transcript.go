// Package transcript reads Claude Code session transcript JSONL: the live context size and the files a session wrote.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/models"
)

// tailBytes bounds the common-case read: the latest usage record sits near EOF of an append-only file.
const tailBytes = 256 << 10

// Usage is the newest assistant usage record: total context tokens and the model that produced it.
type Usage struct {
	Tokens int
	Model  string
}

// LastUsageTokens returns input + cache + output tokens from the last usage record.
func LastUsageTokens(path string) (int, bool) {
	u, ok := LastUsage(path)
	return u.Tokens, ok
}

// LastUsage scans the tail back-to-front, falling back to a full scan when the tail holds no usage line.
func LastUsage(path string) (Usage, bool) {
	f, err := os.Open(path)
	if err != nil || path == "" {
		return Usage{}, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Usage{}, false
	}
	off := max(fi.Size()-tailBytes, 0)
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return Usage{}, false
	}
	for end := len(buf); end > 0; {
		start := bytes.LastIndexByte(buf[:end], '\n') + 1
		if u, ok := usageFromLine(buf[start:end]); ok {
			return u, true
		}
		end = start - 1
	}
	if off == 0 {
		return Usage{}, false
	}
	_, _ = f.Seek(0, io.SeekStart)
	var last Usage
	found := false
	eachLine(f, func(line []byte) {
		if u, ok := usageFromLine(line); ok {
			last, found = u, true
		}
	})
	return last, found
}

var usageKey = []byte(`"usage"`)

func usageFromLine(line []byte) (Usage, bool) {
	if !bytes.Contains(line, usageKey) {
		return Usage{}, false
	}
	var rec struct {
		IsSidechain bool `json:"isSidechain"`
		Message     struct {
			Model string `json:"model"`
			Usage *struct {
				Input         int `json:"input_tokens"`
				CacheRead     int `json:"cache_read_input_tokens"`
				CacheCreation int `json:"cache_creation_input_tokens"`
				Output        int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &rec) != nil || rec.Message.Usage == nil || rec.IsSidechain {
		return Usage{}, false
	}
	u := rec.Message.Usage
	return Usage{Tokens: u.Input + u.CacheRead + u.CacheCreation + u.Output, Model: rec.Message.Model}, true
}

// Window is the context window for a session model: 1M when usage already exceeds 200K or the launch
// env pins a [1m] variant of the model (transcripts record the wire id, never the marker).
func Window(model string, used int) int {
	if used > models.ContextWindowStd {
		return models.ContextWindow1M
	}
	if strings.HasSuffix(model, "[1m]") {
		return models.ContextWindow1M
	}
	for _, k := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL"} {
		v := os.Getenv(k)
		if base := models.Strip1M(v); model != "" && (base == model || strings.HasSuffix(base, ":"+model)) {
			return models.ContextWindow(v)
		}
	}
	return models.ContextWindow(model)
}

var editTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}
var toolUseKey = []byte(`"tool_use"`)

// EditedFiles returns up to limit unique paths the transcript's Edit/Write tool calls touched, sorted.
func EditedFiles(path string, limit int) []string {
	seen := map[string]bool{}
	eachEdit(path, func(p string) {
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
	return out
}

// CountEdits counts every Edit/Write/MultiEdit/NotebookEdit tool_use in the whole transcript (no window:
// re-harvest compares against an earlier count).
func CountEdits(path string) int {
	n := 0
	eachEdit(path, func(string) { n++ })
	return n
}

// eachEdit calls fn with the target path of each editor tool_use in an assistant record.
func eachEdit(path string, fn func(filePath string)) {
	f, err := os.Open(path)
	if err != nil || path == "" {
		return
	}
	defer f.Close()
	eachLine(f, func(line []byte) {
		if !bytes.Contains(line, toolUseKey) {
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
	})
}

// eachLine hands fn a slice valid only until it returns (ReadSlice, no per-line copy); lines longer than the
// buffer are joined into an overflow buffer, so a giant tool-result line never aborts the scan.
func eachLine(r io.Reader, fn func([]byte)) {
	br := bufio.NewReaderSize(r, 1<<20)
	var long []byte
	for {
		line, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			long = append(long, line...)
			continue
		}
		if long != nil {
			line, long = append(long, line...), nil
		}
		if len(line) > 0 {
			fn(line)
		}
		if err != nil {
			return
		}
	}
}
