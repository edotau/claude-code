package frontmatter

import "testing"

// A description block scalar line that happens to read "---" (indented, part of the YAML value) must not be
// mistaken for the closing frontmatter fence, which only matches "---" unindented at line start.
func TestParseBlockScalarContainingDashes(t *testing.T) {
	content := "---\nname: x\ndescription: |\n  intro line\n  ---\n  more text\n---\n\nBody.\n"
	fm, body := Parse(content)
	if fm["name"] != "x" {
		t.Errorf("name = %v, want x", fm["name"])
	}
	if body != "Body." {
		t.Errorf("body = %q, want %q", body, "Body.")
	}
}

// No frontmatter fence at all: the whole input is body, untouched.
func TestParseNone(t *testing.T) {
	content := "Just a plain markdown file.\n\nNo frontmatter here.\n"
	fm, body := Parse(content)
	if len(fm) != 0 {
		t.Errorf("fm = %v, want empty", fm)
	}
	if body != content {
		t.Errorf("body = %q, want %q (unchanged)", body, content)
	}
}

// An opening "---" fence that never closes must not be parsed as frontmatter; the whole input is body.
func TestParseUnclosedFence(t *testing.T) {
	content := "---\nname: x\ndescription: unclosed\nBody follows but no closing fence.\n"
	fm, body := Parse(content)
	if len(fm) != 0 {
		t.Errorf("fm = %v, want empty (unclosed fence is not frontmatter)", fm)
	}
	if body != content {
		t.Errorf("body = %q, want %q (unchanged)", body, content)
	}
}

// Body with no trailing newline after the closing fence.
func TestParseNoTrailingNewline(t *testing.T) {
	content := "---\nname: x\ndescription: d\n---\nBody text, no trailing newline"
	fm, body := Parse(content)
	if fm["name"] != "x" {
		t.Errorf("name = %v, want x", fm["name"])
	}
	if body != "Body text, no trailing newline" {
		t.Errorf("body = %q, want %q", body, "Body text, no trailing newline")
	}
}
