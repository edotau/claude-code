package workflows

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
)

func TestNestedTemplatePreservesWorkflowPins(t *testing.T) {
	src := "const r = await agent(`list:\n${items.map(i => `- first, don't ${i}`).join('\\n')}`, {model: 'sonnet', effort: 'low'})"
	if result := workflowInline(&core.Input{}, src); result.Decision != core.Allow || result.UpdatedInput != nil {
		t.Errorf("pinned inline workflow changed or blocked: %+v", result)
	}
	if hits := lintAllPins("inline", []byte(src)); len(hits) != 0 {
		t.Errorf("pinned template: %v", hits)
	}
	if out, amb := fillModel(src, "sonnet"); out != src || len(amb) != 0 {
		t.Errorf("pinned fillModel = %q, %v", out, amb)
	}
	unpinned := strings.Replace(src, "model: 'sonnet', ", "", 1)
	out, amb := fillModel(unpinned, "sonnet")
	if len(amb) != 0 || len(lintAllPins("filled", []byte(out))) != 0 {
		t.Errorf("unpinned fillModel = %q, %v", out, amb)
	}
	if hits := lintSaved("saved.js", []byte(src)); len(hits) != 0 {
		t.Errorf("saved template: %v", hits)
	}
}
