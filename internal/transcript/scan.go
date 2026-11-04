// Package transcript reads Claude Code session transcript JSONL: the live context size and the files a session wrote.
package transcript

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"sync"
)

// EachLine calls fn per line of r (oversized joined, EOF partial kept); slice valid in fn only; returns non-EOF read errors.
func EachLine(r io.Reader, fn func([]byte)) error {
	_, err := scanLines(r, false, fn)
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

// readerPool reuses the 1MB bufio.Reader across calls — this runs on every Stop hook, so a fresh
// 1<<20 allocation per scan is wasted churn.
var readerPool = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, 1<<20) }}

// scanLines avoids common-case copies and joins oversized lines without stopping the scan.
func scanLines(r io.Reader, completeOnly bool, fn func([]byte)) (int64, error) {
	br := readerPool.Get().(*bufio.Reader)
	br.Reset(r)
	defer readerPool.Put(br)
	var long []byte
	var read int64
	for {
		line, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			long = append(long, line...)
			continue
		}
		if long != nil {
			line, long = append(long, line...), nil
		}
		if len(line) > 0 && (!completeOnly || err == nil) {
			fn(line)
			read += int64(len(line))
		}
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return read, err
		}
	}
}
