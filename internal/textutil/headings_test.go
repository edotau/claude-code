package textutil

import (
	"strings"
	"testing"
)

var benchDoc = []byte(strings.Repeat("# Title\n\nbody text\n\n> quote\n- item\n  - nested\n\n```go\ncode\n```\n<div>\nhtml\n</div>\n\nSetext\n===\n", 200))

func BenchmarkHeadings(b *testing.B) {
	b.SetBytes(int64(len(benchDoc)))
	for b.Loop() {
		Headings(benchDoc)
	}
}
