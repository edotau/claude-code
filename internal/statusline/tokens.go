package statusline

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strconv"
)

// tokenLine is the slice of a transcript record the running sum reads.
type tokenLine struct {
	Type    string `json:"type"`
	Message struct {
		ID    string `json:"id"`
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// sessionTokens is the transcript's cumulative input+output tokens, each response once (the last line of a repeated
// message id wins, as in transcript.ReadResponses). A stamp keeps (offset, sum, open id) so a render reads only new bytes.
func sessionTokens(path string) int {
	if path == "" {
		return 0
	}
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0
	}
	stamp := stampFile("tokens", path)
	var off int64
	var done, last int
	var lastID string
	if s := readStamp(stamp); len(s) == 4 {
		o, e1 := strconv.ParseInt(s[0], 10, 64)
		d, e2 := strconv.Atoi(s[1])
		l, e3 := strconv.Atoi(s[3])
		if e1 == nil && e2 == nil && e3 == nil && o <= fi.Size() {
			off, done, lastID, last = o, d, s[2], l
		}
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return 0
	}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break // a partial trailing line is re-read next render
		}
		off += int64(len(line))
		var rec tokenLine
		if json.Unmarshal(line, &rec) != nil || rec.Type != "assistant" || rec.Message.Usage == nil {
			continue
		}
		n := rec.Message.Usage.InputTokens + rec.Message.Usage.OutputTokens
		if rec.Message.ID == "" || rec.Message.ID != lastID {
			done += last
			lastID = rec.Message.ID
		}
		last = n
	}
	writeStamp(stamp, strconv.FormatInt(off, 10), strconv.Itoa(done), lastID, strconv.Itoa(last))
	return done + last
}
