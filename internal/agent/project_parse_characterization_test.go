package agent

import "testing"

// A "## Memory" heading inside a fenced code block is example text, not a real section, and must survive.
func TestStripMemorySectionInsideFenceNotStripped(t *testing.T) {
	body := "Intro.\n\n```\n## Memory\nexample content, not a real section\n```\n\nTrailer.\n"
	got := stripMemorySection(body)
	if got != body {
		t.Errorf("stripMemorySection stripped a fenced-code '## Memory' line:\ngot:  %q\nwant: %q (unchanged)", got, body)
	}
}
