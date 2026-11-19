package hooks

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/askspec"
)

func TestFixRunLine(t *testing.T) {
	defer askspec.StubSchema(askspec.Schema{Fields: []askspec.Field{
		{Name: "agent", Kind: "string", Usage: "agent"},
		{Name: "model", Kind: "string", Usage: "model id"},
		{Name: "timeout", Kind: "duration", Usage: "limit"},
	}})()

	broken := "Run: claude-code agents ask codex --modle gpt-5 'do it'\nrest of brief"
	got, note := fixRunLine(broken)
	if !strings.Contains(got, "--model") || strings.Contains(got, "--modle") || !strings.HasSuffix(got, "\nrest of brief") {
		t.Fatalf("broken line not fixed: %q", got)
	}
	if !strings.HasPrefix(note, "dispatch-effort: fixed the brief's Run: line") || strings.Count(note, "\n") != 0 {
		t.Fatalf("note = %q", note)
	}

	ok := "Run: claude-code agents ask --agent codex --model gpt-5 'do it'\nrest"
	if got, note := fixRunLine(ok); got != ok || note != "" {
		t.Fatalf("correct line changed: %q, note %q", got, note)
	}

	multi := "Run: claude-code agents ask --modle gpt-5 'line one\nline two'"
	if got, note := fixRunLine(multi); got != multi || !strings.Contains(note, "spans lines") {
		t.Fatalf("multi-line command changed: %q, note %q", got, note)
	}
}

func TestToolsNote(t *testing.T) {
	brief := "Run: claude-code agents ask --agent opencode -\nYou OWN pkg/a.go; gate: rtk go test ./pkg"
	if !strings.Contains(toolsNote(brief), "--tools") {
		t.Fatalf("tool-needing brief without --tools: no note")
	}
	withTools := "Run: claude-code agents ask --agent opencode --tools -\nYou OWN pkg/a.go"
	readOnly := "Run: claude-code agents ask --agent opencode -\nSummarize the design in five lines."
	for _, b := range []string{withTools, readOnly, "no run line\nrtk go test"} {
		if n := toolsNote(b); n != "" {
			t.Fatalf("toolsNote(%q) = %q, want none", b, n)
		}
	}
}
