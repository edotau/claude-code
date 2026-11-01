package agent

import (
	"bufio"
	"strings"
)

func newBR(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }

type nonJSONCase struct {
	name   string
	events string
	big    func(text string) string                        // one event carrying text
	run    func(string, StreamLog) (Result, string, error) // returns the result and the streamed text
}



