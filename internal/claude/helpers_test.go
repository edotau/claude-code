package claude

import (
	"bufio"
	"io"
	"strings"
)

func newBR(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }

type failingStreamWriter struct{}

func (failingStreamWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
