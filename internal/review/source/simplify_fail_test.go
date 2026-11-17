package source

import (
	"strings"
	"testing"
)

func BenchmarkCommentViolations(b *testing.B) {
	for _, prose := range []string{"// One line.\n", "// First line.\n// Second line.\n"} {
		b.Run(strings.TrimSpace(prose), func(b *testing.B) {
			content := "package fixture\n" + strings.Repeat("\n"+prose+"func F() {}\n", 200)
			spans := lineSpans{{1, 1 << 30}}
			b.ReportAllocs()
			for b.Loop() {
				commentViolations(content, content, spans)
			}
		})
	}
}
