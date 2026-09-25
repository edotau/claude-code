package crossgen

import "testing"

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
