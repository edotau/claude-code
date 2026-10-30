package textutil

import (
	"bytes"
	"testing"

	"github.com/jeduden/mdsmith/pkg/markdown"
)

// stripFrontMatter must match mdsmith's StripFrontMatter byte for byte (the dependency it replaces).
func TestStripFrontMatterMatchesMdsmith(t *testing.T) {
	cases := []string{
		"", "plain body\n", "x\n---\na: b\n---\nbody\n", "---\na: b\nbody only, unterminated\n",
		"---  \na: b\n---\nbody\n", "---\na: b\n---  \nbody\n", "---\n---\nbody\n", "---\n---\n", "---\n", "---",
		"---\na: b\n...\nbody\n", "---\na: b\n---", "---\na: b\n---\nbody\n---\nmore\n", "---\na: |\n  ---\n  x\n---\nb\n",
		"---\n\n---\nbody\n", "---\r\na: b\r\n---\r\n",
	}
	for _, c := range cases {
		wp, wc := markdown.StripFrontMatter([]byte(c))
		gp, gc := stripFrontMatter([]byte(c))
		if (wp == nil) != (gp == nil) || !bytes.Equal(wp, gp) || !bytes.Equal(wc, gc) {
			t.Errorf("%q: got (%q,%q), want (%q,%q)", c, gp, gc, wp, wc)
		}
	}
}
