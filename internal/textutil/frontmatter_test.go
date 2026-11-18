package textutil

import (
	"slices"
	"testing"
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

func TestParseFrontmatterInlineListQuotedComma(t *testing.T) {
	fm, _ := ParseFrontmatter("---\nnames: [\"alpha,beta\"]\n---\n")
	names, ok := fm["names"].([]string)
	if !ok || !slices.Equal(names, []string{"alpha,beta"}) {
		t.Fatalf("names = %#v, want [\"alpha,beta\"]", fm["names"])
	}
}

func TestParseFrontmatterCommentInsideUnindentedList(t *testing.T) {
	fm, _ := ParseFrontmatter("---\ntools:\n- Read\n# comment\n- Write\n---\n")
	tools, ok := fm["tools"].([]string)
	if !ok || !slices.Equal(tools, []string{"Read", "Write"}) {
		t.Fatalf("tools = %#v, want [Read Write]", fm["tools"])
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

// StripFrontMatter anchors the closing fence at a line start and never treats a bare or CRLF fence as front matter.
func TestStripFrontMatterFences(t *testing.T) {
	cases := []struct {
		in      string
		prefix  *string
		content string
	}{
		{"", nil, ""},
		{"plain body\n", nil, "plain body\n"},
		{"x\n---\na: b\n---\nbody\n", nil, "x\n---\na: b\n---\nbody\n"},
		{"---\na: b\nbody only, unterminated\n", nil, "---\na: b\nbody only, unterminated\n"},
		{"---  \na: b\n---\nbody\n", nil, "---  \na: b\n---\nbody\n"},
		{"---\na: b\n---  \nbody\n", nil, "---\na: b\n---  \nbody\n"},
		{"---\n---\nbody\n", new("---\n---\n"), "body\n"},
		{"---\n---\n", new("---\n---\n"), ""},
		{"---\n", nil, "---\n"},
		{"---", nil, "---"},
		{"---\na: b\n...\nbody\n", nil, "---\na: b\n...\nbody\n"},
		{"---\na: b\n---", nil, "---\na: b\n---"},
		{"---\na: b\n---\nbody\n---\nmore\n", new("---\na: b\n---\n"), "body\n---\nmore\n"},
		{"---\na: |\n  ---\n  x\n---\nb\n", new("---\na: |\n  ---\n  x\n---\n"), "b\n"},
		{"---\n\n---\nbody\n", new("---\n\n---\n"), "body\n"},
		{"---\r\na: b\r\n---\r\n", nil, "---\r\na: b\r\n---\r\n"},
	}
	for _, c := range cases {
		gp, gc := StripFrontMatter([]byte(c.in))
		if (gp == nil) != (c.prefix == nil) || (c.prefix != nil && string(gp) != *c.prefix) || string(gc) != c.content {
			t.Errorf("%q: got (%q,%q), want (%v,%q)", c.in, gp, gc, c.prefix, c.content)
		}
	}
}

// An indented "---" inside a description block scalar is not the closing fence, which only matches at line start.
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

// A closing fence that is the last bytes of the file (no trailing newline) still ends the frontmatter.
func TestParseFrontmatterClosingFenceAtEOF(t *testing.T) {
	fm, body := ParseFrontmatter("---\nname: x\n---")
	if fm["name"] != "x" {
		t.Errorf("fm = %v, want name=x", fm)
	}
	if body != "" {
		t.Errorf("body = %q, want empty", body)
	}
}

// A block list whose "- item" lines sit at the key's own indent is valid YAML and must keep its items.
func TestParseFrontmatterUnindentedBlockList(t *testing.T) {
	content := "---\nname: x\ntools:\n- Read\n- \"Grep\"\nmodel: opus\n---\nbody\n"
	fm, _ := ParseFrontmatter(content)
	tools, _ := fm["tools"].([]string)
	if len(tools) != 2 || tools[0] != "Read" || tools[1] != "Grep" {
		t.Errorf("tools = %#v, want [Read Grep]", fm["tools"])
	}
	if fm["model"] != "opus" {
		t.Errorf("model = %v, want opus", fm["model"])
	}
}

// stripQuotes removes one matching surrounding pair only; inner quotes survive.
func TestParseFrontmatterKeepsInnerQuotes(t *testing.T) {
	fm, _ := ParseFrontmatter("---\ntitle: 'say \"hi\"'\n---\nBody.\n")
	if fm["title"] != `say "hi"` {
		t.Errorf("title = %q, want %q", fm["title"], `say "hi"`)
	}
}

// A bare `key:` with no list items is an empty string; items turn it into a list.
func TestParseFrontmatterBareKeyIsEmptyString(t *testing.T) {
	fm, _ := ParseFrontmatter("---\nempty:\nname: x\ntags:\n  - a\n---\nBody.\n")
	if fm["empty"] != "" {
		t.Errorf("empty = %#v, want \"\"", fm["empty"])
	}
	if tags, ok := fm["tags"].([]string); !ok || len(tags) != 1 || tags[0] != "a" {
		t.Errorf("tags = %#v, want [a]", fm["tags"])
	}
}

func TestParseFrontmatterNestedMetadataKeys(t *testing.T) {
	fm, _ := ParseFrontmatter("---\nname: n\nmetadata:\n  type: feedback\n  modified: 2026-11-14T00:21:38.852Z\n  pinned: true\ndescription: >\n  Note: folded\n---\nbody\n")
	meta, ok := fm["metadata"].(map[string]any)
	if !ok || meta["type"] != "feedback" || meta["modified"] != "2026-11-14T00:21:38.852Z" || meta["pinned"] != true {
		t.Errorf("metadata = %#v, want type, modified and pinned read", fm["metadata"])
	}
	if fm["description"] != "Note: folded" {
		t.Errorf("a block scalar line that looks like k: v must stay folded, got %#v", fm["description"])
	}
}
