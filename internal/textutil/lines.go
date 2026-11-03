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

// EachLine joins oversized lines and includes the final partial line; callback bytes exclude CRLF and are borrowed.
// Read errors other than EOF end iteration silently; use EachLineErr to see them.
func EachLine(r io.Reader, fn func(line []byte)) { _ = EachLineErr(r, fn) }

// EachLineErr is EachLine but returns the first non-EOF read error.
func EachLineErr(r io.Reader, fn func(line []byte)) error {
	br := lineReaderPool.Get().(*bufio.Reader)
	br.Reset(r)
	defer func() {
		br.Reset(nil)
		lineReaderPool.Put(br)
	}()
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
			line = bytes.TrimSuffix(line, []byte{'\n'})
			fn(bytes.TrimSuffix(line, []byte{'\r'}))
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}
