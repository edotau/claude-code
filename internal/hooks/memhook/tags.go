package memhook

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/textutil"
)

// tagLockWait bounds how long a Stop waits for a peer Stop of the same session.
const tagLockWait = 2 * time.Second

// recordTags appends the assistant tag lines written since the last Stop as typed events; best effort, never fatal.
func recordTags(session, transcriptPath string, now time.Time) {
	if session == "" || transcriptPath == "" {
		return
	}
	logPath := memory.TagEventsPath(session)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return
	}
	_, _ = paths.TryFileLock(logPath+".lock", tagLockWait, func() error {
		scanTags(logPath, transcriptPath, now)
		return nil
	})
}

// scanTags reads the transcript's complete lines past the saved offset and logs their events.
func scanTags(logPath, transcriptPath string, now time.Time) {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return
	}
	off := readOffset(logPath + ".off")
	if off > st.Size() { // a rewritten (compacted) transcript restarts the scan
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return
	}
	events, read := tagEventsIn(f, now)
	if len(events) > 0 && !appendEvents(logPath, events) {
		return
	}
	// No fsync: a torn offset reads as 0 and the rescan's events dedup against the log; an unterminated tail is re-read.
	_ = paths.AtomicWriteNoSync(logPath+".off", []byte(strconv.FormatInt(off+read, 10)), 0o600)
}

func readOffset(path string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// textType and assistantType are values every assistant text-block line carries; transcripts never escape ASCII.
var textType, assistantType = []byte(`"text"`), []byte(`"assistant"`)

// tagEventsIn extracts the assistant text-block tag lines of complete transcript lines, in order, and the bytes read.
func tagEventsIn(r io.Reader, now time.Time) ([]memory.TagEvent, int64) {
	var out []memory.TagEvent
	at := now.UTC().Format(time.RFC3339)
	read, _ := textutil.ScanRawLines(r, true, func(line []byte) {
		for _, block := range assistantBlocks(line) {
			if block.Type != "text" {
				continue
			}
			for ln := range strings.SplitSeq(block.Text, "\n") {
				if kind, text, ok := memory.ParseTagLine(ln); ok && text != "" {
					out = append(out, memory.TagEvent{Event: strings.ToLower(kind), Text: text, At: at})
				}
			}
		}
	})
	return out, read
}

// contentBlock is the part of one message content block tag extraction reads.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// assistantBlocks is an assistant transcript line's content blocks; nil for any other line.
func assistantBlocks(line []byte) []contentBlock {
	if !bytes.Contains(line, textType) || !bytes.Contains(line, assistantType) { // fast reject: no assistant text block
		return nil
	}
	var rec struct {
		Type    string `json:"type"`
		Message struct {
			Role    string         `json:"role"`
			Content []contentBlock `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &rec) != nil || rec.Type != "assistant" || rec.Message.Role != "assistant" {
		return nil
	}
	return rec.Message.Content
}

// appendEvents adds events not already in the log; false when the log cannot be written.
func appendEvents(logPath string, events []memory.TagEvent) bool {
	seen := map[string]bool{}
	if b, err := os.ReadFile(logPath); err == nil {
		for ln := range bytes.SplitSeq(b, []byte("\n")) {
			var e memory.TagEvent
			if json.Unmarshal(ln, &e) == nil {
				seen[e.Event+"\x00"+e.Text] = true
			}
		}
	}
	var buf bytes.Buffer
	for _, e := range events {
		key := e.Event + "\x00" + e.Text
		if seen[key] {
			continue
		}
		seen[key] = true
		b, _ := json.Marshal(e)
		buf.Write(append(b, '\n'))
	}
	if buf.Len() == 0 {
		return true
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	_, werr := f.Write(buf.Bytes())
	return f.Close() == nil && werr == nil
}
