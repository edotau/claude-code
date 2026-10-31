package textutil

import (
	"bytes"
	"testing"

	"github.com/jeduden/mdsmith/pkg/markdown"
)

// A CRLF (or BOM-prefixed) agent.md must not silently parse as all-body — that drops `name:`.
func TestParseFrontmatterCRLFAndBOM(t *testing.T) {
	content := "\ufeff---\r\nname: claude\r\ndescription: builds things\r\n---\r\nBody text.\r\n"
	fm, body := ParseFrontmatter(content)
	if fm["name"] != "claude" {
		t.Errorf("name = %v, want claude", fm["name"])
	}
	if fm["description"] != "builds things" {
		t.Errorf("description = %v, want 'builds things'", fm["description"])
	}
	if body != "Body text." {
		t.Errorf("body = %q, want %q", body, "Body text.")
	}
}

// Quoted scalar and list-item values must have their quotes stripped, both kinds.
func TestParseFrontmatterQuotedValues(t *testing.T) {
	content := "---\n" +
		"name: \"claude\"\n" +
		"title: 'agent one'\n" +
		"tags:\n" +
		"  - \"alpha\"\n" +
		"  - 'beta'\n" +
		"---\nBody.\n"
	fm, _ := ParseFrontmatter(content)
	if fm["name"] != "claude" {
		t.Errorf("name = %v, want claude (quotes stripped)", fm["name"])
	}
	if fm["title"] != "agent one" {
		t.Errorf("title = %v, want 'agent one' (quotes stripped)", fm["title"])
	}
	tags, ok := fm["tags"].([]string)
	if !ok || len(tags) != 2 || tags[0] != "alpha" || tags[1] != "beta" {
		t.Errorf("tags = %v, want [alpha beta] (quotes stripped)", fm["tags"])
	}
}

// Inline-bracket list values quote their items too.
func TestParseFrontmatterQuotedInlineList(t *testing.T) {
	content := "---\nnames: [\"a\", 'b', c]\n---\nBody.\n"
	fm, _ := ParseFrontmatter(content)
	names, ok := fm["names"].([]string)
	if !ok || len(names) != 3 || names[0] != "a" || names[1] != "b" || names[2] != "c" {
		t.Errorf("names = %v, want [a b c]", fm["names"])
	}
}

// A duplicate key keeps the LAST value seen, not the first.
func TestParseFrontmatterDuplicateKeyKeepsLast(t *testing.T) {
	content := "---\nname: first\nname: second\n---\nBody.\n"
	fm, _ := ParseFrontmatter(content)
	if fm["name"] != "second" {
		t.Errorf("name = %v, want second (last value wins)", fm["name"])
	}
}

// An indented continuation line (no list marker) joins onto the current key's string value.
func TestParseFrontmatterContinuationLineJoinsDescription(t *testing.T) {
	content := "---\ndescription: first line\n  second line\n---\nBody.\n"
	fm, _ := ParseFrontmatter(content)
	if fm["description"] != "first line second line" {
		t.Errorf("description = %q, want %q", fm["description"], "first line second line")
	}
}

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

// A description block scalar line that happens to read "---" (indented, part of the YAML value) must not be
// mistaken for the closing frontmatter fence, which only matches "---" unindented at line start.
func TestParseFrontmatterBlockScalarContainingDashes(t *testing.T) {
	content := "---\nname: x\ndescription: |\n  intro line\n  ---\n  more text\n---\n\nBody.\n"
	fm, body := ParseFrontmatter(content)
	if fm["name"] != "x" {
		t.Errorf("name = %v, want x", fm["name"])
	}
	if body != "Body." {
		t.Errorf("body = %q, want %q", body, "Body.")
	}
}

// No frontmatter fence at all: the whole input is body, untouched.
func TestParseFrontmatterNone(t *testing.T) {
	content := "Just a plain markdown file.\n\nNo frontmatter here.\n"
	fm, body := ParseFrontmatter(content)
	if len(fm) != 0 {
		t.Errorf("fm = %v, want empty", fm)
	}
	if body != content {
		t.Errorf("body = %q, want %q (unchanged)", body, content)
	}
}

// An opening "---" fence that never closes must not be parsed as frontmatter; the whole input is body.
func TestParseFrontmatterUnclosedFence(t *testing.T) {
	content := "---\nname: x\ndescription: unclosed\nBody follows but no closing fence.\n"
	fm, body := ParseFrontmatter(content)
	if len(fm) != 0 {
		t.Errorf("fm = %v, want empty (unclosed fence is not frontmatter)", fm)
	}
	if body != content {
		t.Errorf("body = %q, want %q (unchanged)", body, content)
	}
}

// Body with no trailing newline after the closing fence.
func TestParseFrontmatterNoTrailingNewline(t *testing.T) {
	content := "---\nname: x\ndescription: d\n---\nBody text, no trailing newline"
	fm, body := ParseFrontmatter(content)
	if fm["name"] != "x" {
		t.Errorf("name = %v, want x", fm["name"])
	}
	if body != "Body text, no trailing newline" {
		t.Errorf("body = %q, want %q", body, "Body text, no trailing newline")
	}
}
