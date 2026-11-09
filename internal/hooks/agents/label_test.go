package agents

import (
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/settings"
)

// A Plan leaf runs on the session model: unset it labels with the session id, an explicit pin wins, and no session labels nothing.
func TestDispatchLabelPlanUsesSessionModel(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	stubLabelModels(t, "", "")
	label, model, pressure := DispatchLabel(LabelInput{Type: "Plan", SessionModel: "databricks-claude-opus-5-5"})
	if label != "databricks-claude-opus-5-5" || model != "" || pressure {
		t.Fatalf("Plan without a brief = (%q, %q, %v), want the session id, no pin, no pressure", label, model, pressure)
	}
	if label, _, _ := DispatchLabel(LabelInput{Type: "Plan", Model: "opus", SessionModel: "s"}); label != "opus" {
		t.Errorf("pinned Plan label = %q, want the pin opus", label)
	}
	if label, _, _ := DispatchLabel(LabelInput{Type: "Plan"}); label != "" {
		t.Errorf("Plan with no session = %q, want no label", label)
	}
}

// A brief with no pin is classified: the label is the classifier's model, the same as the hook writes.
func TestDispatchLabelClassifiesBrief(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	stubLabelModels(t, "", "")
	label, model, _ := DispatchLabel(LabelInput{Type: "Plan", SessionModel: "s", Brief: "rename one variable"})
	if model == "" || label != model {
		t.Errorf("classified brief = (label %q, model %q), want the pinned model as the label", label, model)
	}
}

func TestDispatchLabelPreservesModelUnderGemini(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(settings.AgentEnv, core.AgentGemini)
	stubLabelModels(t, "", "")
	label, model, pressure := DispatchLabel(LabelInput{Type: "Plan", Model: "opus", Brief: "rename one variable"})
	if label != "opus" || model != "opus" || pressure {
		t.Fatalf("Gemini ambient agent = (%q, %q, %v), want (opus, opus, false)", label, model, pressure)
	}
	if got := os.Getenv(settings.AgentEnv); got != core.AgentGemini {
		t.Errorf("ambient agent after labeling = %q, want %q", got, core.AgentGemini)
	}
}

func TestDispatchLabelPressureWithoutModelChange(t *testing.T) {
	withPressure(t, 95)
	t.Setenv("HARNESS_ROUTE_SUBAGENTS", "1")
	stubLabelModels(t, "", "")
	if _, _, active := pressureActive(); !active {
		t.Fatal("fixture must activate plan pressure")
	}
	label, model, pressure := DispatchLabel(LabelInput{Type: "Plan", SessionModel: "session-model"})
	if label != "session-model" || model != "" || pressure {
		t.Fatalf("unchanged dispatch under pressure = (%q, %q, %v), want (session-model, empty, false)", label, model, pressure)
	}
}
