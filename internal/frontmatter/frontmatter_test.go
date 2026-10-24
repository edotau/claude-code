package frontmatter

import "testing"

// A CRLF (or BOM-prefixed) agent.md must not silently parse as all-body — that drops `name:`.
func TestParseCRLFAndBOM(t *testing.T) {
	content := "\ufeff---\r\nname: claude\r\ndescription: builds things\r\n---\r\nBody text.\r\n"
	fm, body := Parse(content)
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
func TestParseQuotedValues(t *testing.T) {
	content := "---\n" +
		"name: \"claude\"\n" +
		"title: 'agent one'\n" +
		"tags:\n" +
		"  - \"alpha\"\n" +
		"  - 'beta'\n" +
		"---\nBody.\n"
	fm, _ := Parse(content)
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
func TestParseQuotedInlineList(t *testing.T) {
	content := "---\nnames: [\"a\", 'b', c]\n---\nBody.\n"
	fm, _ := Parse(content)
	names, ok := fm["names"].([]string)
	if !ok || len(names) != 3 || names[0] != "a" || names[1] != "b" || names[2] != "c" {
		t.Errorf("names = %v, want [a b c]", fm["names"])
	}
}

// A duplicate key keeps the LAST value seen, not the first.
func TestParseDuplicateKeyKeepsLast(t *testing.T) {
	content := "---\nname: first\nname: second\n---\nBody.\n"
	fm, _ := Parse(content)
	if fm["name"] != "second" {
		t.Errorf("name = %v, want second (last value wins)", fm["name"])
	}
}

// An indented continuation line (no list marker) joins onto the current key's string value.
func TestParseContinuationLineJoinsDescription(t *testing.T) {
	content := "---\ndescription: first line\n  second line\n---\nBody.\n"
	fm, _ := Parse(content)
	if fm["description"] != "first line second line" {
		t.Errorf("description = %q, want %q", fm["description"], "first line second line")
	}
}
