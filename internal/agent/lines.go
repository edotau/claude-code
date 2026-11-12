package agent

import (
	"bufio"
	"io"
)

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
