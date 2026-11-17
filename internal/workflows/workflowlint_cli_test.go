package workflows

import (
	"testing"
)

func BenchmarkLintAllPins(b *testing.B) {
	data := []byte("const result = agent('Review the fixture', { model: 'opus' })\n")
	b.ReportAllocs()
	for b.Loop() {
		lintAllPins("code-engineer.js", data)
	}
}
