package textutil

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"sync"
)

// FirstLine returns the text before the first newline without trimming whitespace.
func FirstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

var lineReaderPool = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, 1<<20) }}

// EachLineErr joins oversized lines and includes the final partial line; callback bytes exclude CRLF and are borrowed.
// It returns the first non-EOF read error.
func EachLineErr(r io.Reader, fn func(line []byte)) error {
	_, err := ScanRawLines(r, false, func(line []byte) {
		line = bytes.TrimSuffix(line, []byte{'\n'})
		fn(bytes.TrimSuffix(line, []byte{'\r'}))
	})
	return err
}

// ScanRawLines is EachLineErr keeping each terminator; completeOnly drops an unterminated tail; returns bytes read.
func ScanRawLines(r io.Reader, completeOnly bool, fn func(line []byte)) (int64, error) {
	br := lineReaderPool.Get().(*bufio.Reader)
	br.Reset(r)
	defer func() {
		br.Reset(nil)
		lineReaderPool.Put(br)
	}()
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

// ReadLineCapped returns the next line with its newline; a line over max bytes is drained and flagged tooLong.
func ReadLineCapped(r *bufio.Reader, max int) (line []byte, tooLong bool, err error) {
	for {
		chunk, err := r.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > max {
				tooLong, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		switch err {
		case bufio.ErrBufferFull:
			continue
		case nil:
			return line, tooLong, nil
		case io.EOF:
			if len(line) > 0 || tooLong {
				return line, tooLong, io.EOF
			}
		}
		return nil, false, err
	}
}
