package textutil

import (
	"fmt"
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

// Headings finds the headings a CommonMark parse finds: fences, comments and indented code hide "#"; setext counts.
func TestHeadings(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"atx levels and closing hashes", "# a\n## b ##\n### c #x\n#### d\\#\n", "1:a|2:b|3:c #x|4:d\\#"},
		{"not headings", "#x\n####### seven\n    ## code\n\\## esc\n", ""},
		{"empty atx", "##\n## \n### ###\n", "2:|2:|3:"},
		{"backtick fence", "```go\n## in\n```\n## out\n", "2:out"},
		{"tilde fence longer than three", "~~~~\n## in\n~~~\n## still in\n~~~~\n## out\n", "2:out"},
		{"fence inside fence needs matching char", "```\n~~~\n## in\n```\n## out\n", "2:out"},
		{"indented fence", "   ```\n## in\n   ```\n## out\n", "2:out"},
		{"unclosed fence", "```\n## in\n", ""},
		{"setext", "Title\n=====\nSub\n---\n", "1:Title|2:Sub"},
		{"thematic break is not setext", "text\n\n---\n## h\n", "2:h"},
		{"html comment hides", "<!--\n## in\n-->\n## out\n", "2:out"},
		{"quote and list", "> ## q\n- ## l\n", "2:q|2:l"},
		{"crlf", "# a\r\n## b\r\n```\r\n## in\r\n```\r\n", "1:a|2:b"},
		{"lone angle bracket line", "<\na\n<\n> <\n- <\n## h\n", "2:h"},
	}
	for _, c := range cases {
		var got []string
		for _, h := range Headings([]byte(c.in)) {
			got = append(got, fmt.Sprintf("%d:%s", h.Level, h.Text))
		}
		if g := strings.Join(got, "|"); g != c.want {
			t.Errorf("%s: %q -> %q, want %q", c.name, c.in, g, c.want)
		}
	}
}
