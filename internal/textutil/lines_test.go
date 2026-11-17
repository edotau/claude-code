package textutil

import (
	"bytes"
	"testing"
)

func BenchmarkScanRawLines(b *testing.B) {
	b.SetBytes(int64(len(benchDoc)))
	for b.Loop() {
		if _, err := ScanRawLines(bytes.NewReader(benchDoc), true, func([]byte) {}); err != nil {
			b.Fatal(err)
		}
	}
}
