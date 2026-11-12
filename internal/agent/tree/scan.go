package tree

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/transcript"
)

// updateTitlePrefix is the background-update system prompt, never a real user ask.
const updateTitlePrefix = "Background update of a finished session"

// isSkippableTitle rejects a tag-wrapped harness directive or the SessionEnd update prompt as a title.
func isSkippableTitle(text string) bool {
	return strings.HasPrefix(text, "<") || strings.HasPrefix(text, updateTitlePrefix)
}

// headScanBytes bounds the main-transcript head read: cwd/branch/title appear in the first few records.
const headScanBytes = 64 << 10

// activityScanBytes bounds the subagent tail read for the newest tool/text activity signal.
const activityScanBytes = 64 << 10

// isJSONNull reports whether raw is the literal JSON null (vs. absent/empty, which is "").
func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// sessionHead is what the first ~64KB of a main transcript carries about the session itself.
type sessionHead struct {
	cwd, branch, title string
}

// sessionHeadLine is the head-record subset; cwd/gitBranch may ride on any line type, so the first carrier wins.
type sessionHeadLine struct {
	Type        string          `json:"type"`
	Cwd         string          `json:"cwd"`
	GitBranch   string          `json:"gitBranch"`
	IsSidechain bool            `json:"isSidechain"`
	Message     json.RawMessage `json:"message"`
}

// newLineScanner scans r by line with a 64KB initial and 1MB max buffer.
func newLineScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	return sc
}

// scanSessionHead streams the first headScanBytes of path for cwd, branch and the first human user turn as title.
func scanSessionHead(path string) sessionHead {
	var head sessionHead
	f, err := os.Open(path)
	if err != nil {
		return head
	}
	defer f.Close()

	sc := newLineScanner(io.LimitReader(f, headScanBytes))
	for sc.Scan() {
		var rec sessionHeadLine
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		head.cwd = cmp.Or(head.cwd, rec.Cwd)
		head.branch = cmp.Or(head.branch, rec.GitBranch)
		if head.title == "" {
			head.title = titleFromRecord(rec)
		}
		if head.cwd != "" && head.branch != "" && head.title != "" {
			break
		}
	}
	return head
}

// titleFromRecord returns the truncated human user-turn text of rec, "" when rec is not a usable title source.
func titleFromRecord(rec sessionHeadLine) string {
	if rec.Type != "user" || rec.IsSidechain {
		return ""
	}
	text := strings.TrimSpace(userTurnText(rec.Message))
	if text == "" || isSkippableTitle(text) {
		return ""
	}
	return truncate(text, 60)
}

// userTurnText reads a user record's message.content: a plain string or an array of blocks (text parts concatenated).
func userTurnText(raw json.RawMessage) string {
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return ""
	}
	return transcript.ContentText(msg.Content)
}

// truncate renders s on one line, cut to n runes with an ellipsis when longer.
func truncate(s string, n int) string {
	return textutil.CapRunes(oneLine(s), n, "…")
}

// oneLine collapses embedded newlines/tabs and repeated spaces so a multi-line prompt renders as
// one title line.
func oneLine(s string) string {
	out := make([]rune, 0, len(s))
	prevSpace := false
	for _, c := range s {
		if c == '\n' || c == '\r' || c == '\t' {
			c = ' '
		}
		if c == ' ' && prevSpace {
			continue
		}
		prevSpace = c == ' '
		out = append(out, c)
	}
	return string(out)
}

var toolUseMarker = []byte(`"tool_use"`)

// assistantContentLine is the assistant record subset this file reads: tool_use/text blocks plus the timestamp.
type assistantContentLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
			Text  string          `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

// parseRFC3339 parses a transcript record's timestamp, zero on failure.
func parseRFC3339(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// tailBuf reads path's last n bytes (the whole file when smaller), nil on any read failure.
func tailBuf(path string, n int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	off := max(fi.Size()-n, 0)
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return nil
	}
	return buf
}

// lastActivity tail-scans path for the newest assistant tool_use (Tool+Arg) and text block, paths shortened.
func lastActivity(path, cwd string) Activity {
	buf := tailBuf(path, activityScanBytes)
	if buf == nil {
		return Activity{}
	}
	home, _ := os.UserHomeDir()
	var act Activity
	for raw := range bytes.SplitSeq(buf, []byte("\n")) {
		applyEvents(&act, raw, home, cwd)
	}
	return act
}

// toolArg extracts the one input field most useful to show, in priority order: file_path, path,
// command (truncated to 60 runes, for Bash), pattern (Grep/Glob).
func toolArg(input json.RawMessage) string {
	var in struct {
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
		Command  string `json:"command"`
		Pattern  string `json:"pattern"`
	}
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	switch {
	case in.FilePath != "":
		return in.FilePath
	case in.Path != "":
		return in.Path
	case in.Command != "":
		return truncate(in.Command, 60)
	default:
		return in.Pattern
	}
}

// stripPaths removes the session cwd and collapses $HOME to "~" wherever either appears in s.
func stripPaths(s, home, cwd string) string {
	if cwd != "" && cwd != "/" { // a root cwd would delete every slash
		s = strings.ReplaceAll(s, cwd+"/", "")
		s = strings.ReplaceAll(s, cwd, "")
	}
	if home != "" {
		s = strings.ReplaceAll(s, home, "~")
	}
	return s
}

// applyEvents folds one assistant transcript record's tool_use/text blocks into act, the last of each kind winning.
func applyEvents(act *Activity, raw []byte, home, cwd string) {
	if !bytes.Contains(raw, toolUseMarker) && !bytes.Contains(raw, []byte(`"text"`)) {
		return
	}
	var rec assistantContentLine
	if json.Unmarshal(raw, &rec) != nil || rec.Type != "assistant" {
		return
	}
	at := parseRFC3339(rec.Timestamp)
	for _, c := range rec.Message.Content {
		switch {
		case c.Type == "tool_use" && c.Name != "":
			act.Tool, act.Arg, act.At = c.Name, stripPaths(toolArg(c.Input), home, cwd), at
		case c.Type == "text" && c.Text != "":
			act.Text, act.At = truncate(c.Text, 80), at
		}
	}
}
