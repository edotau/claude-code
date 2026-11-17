package source

import (
	"strings"
	"testing"
)

func BenchmarkScanCommentBlocks(b *testing.B) {
	content := "package fixture\n" + strings.Repeat("\n// One line.\nfunc F() {}\n", 200)
	b.ReportAllocs()
	b.SetBytes(int64(len(content)))
	for b.Loop() {
		scanCommentBlocks(content, "go")
	}
}
