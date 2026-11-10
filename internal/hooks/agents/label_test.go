package agents

import (
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/settings"
)

// A Plan leaf runs on the session model: unset it labels with the session id, an explicit pin wins, and no session labels nothing.
func TestDispatchLabelPlanUsesSessionModel(t *testing.T) {
	useAgentsDir(t, "Plan")
	stubLabelModels(t, "", "")
	label, model, _ := dispatchLabel(LabelInput{Type: "Plan", SessionModel: "databricks-claude-opus-5-5"})
	if label != "databricks-claude-opus-5-5" || model != "" {
		t.Fatalf("Plan without a brief = (%q, %q), want the session id, no pin", label, model)
	}
	if label, _, _ := dispatchLabel(LabelInput{Type: "Plan", Model: "opus", SessionModel: "s"}); label != "opus" {
		t.Errorf("pinned Plan label = %q, want the pin opus", label)
	}
	if label, _, _ := dispatchLabel(LabelInput{Type: "Plan"}); label != "" {
		t.Errorf("Plan with no session = %q, want no label", label)
	}
}

// A brief with no pin is classified: the label is the classifier's model, the same as the hook writes.
func TestDispatchLabelClassifiesBrief(t *testing.T) {
	useAgentsDir(t, "Plan")
	stubLabelModels(t, "", "")
	label, model, _ := dispatchLabel(LabelInput{Type: "Plan", SessionModel: "s", Brief: "rename one variable"})
	if model == "" || label != model {
		t.Errorf("classified brief = (label %q, model %q), want the pinned model as the label", label, model)
	}
}

func TestDispatchLabelPreservesModelUnderGemini(t *testing.T) {
	useAgentsDir(t, "Plan")
	t.Setenv(settings.AgentEnv, core.AgentGemini)
	stubLabelModels(t, "", "")
	label, model, _ := dispatchLabel(LabelInput{Type: "Plan", Model: "opus", Brief: "rename one variable"})
	if label != "opus" || model != "opus" {
		t.Fatalf("Gemini ambient agent = (%q, %q), want (opus, opus)", label, model)
	}
	if got := os.Getenv(settings.AgentEnv); got != core.AgentGemini {
		t.Errorf("ambient agent after labeling = %q, want %q", got, core.AgentGemini)
	}
}

// Source names where the label's model came from: the dispatch's --model, the session, or the pin and route.
func TestLabelSource(t *testing.T) {
	for _, c := range []struct{ dispatch, label, session, want string }{
		{"opus", "opus", "s", "dispatch"}, {"inherit", "s", "s", "session"},
		{"", "s", "s", "session"}, {"", "sonnet", "s", "derived"}, {"", "", "", "derived"},
		{"opus", "databricks-gpt-6-1-sol", "sess-x", "derived"},
		{"opus", "sess-x", "sess-x", "session"},
		{"opus", "", "sess-x", "derived"},
	} {
		if got := labelSource(c.dispatch, c.label, c.session); got != c.want {
			t.Errorf("labelSource(%q,%q,%q) = %q, want %q", c.dispatch, c.label, c.session, got, c.want)
		}
	}
}
