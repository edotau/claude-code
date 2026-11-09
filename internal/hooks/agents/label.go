package agents

import (
	"bytes"
	"encoding/json"
	"maps"

	"github.com/edotau/claude-code/internal/hooks/core"
)

// LabelInput is one dispatch to label: its leaf type, model pin ("" = unset), brief and the session's model id.
type LabelInput struct {
	Type, Model, Brief, SessionModel string
}

// labelDescription stands in for the description: leafLabel needs a non-empty one, the prefix does not depend on it.
const labelDescription = "task"

// DispatchLabel runs the dispatch-effort pin and label path on a synthetic spawn: the prefix, the model the dispatch
// carries after the pin, and whether plan pressure would downshift that model when the hook runs.
func DispatchLabel(in LabelInput) (label, model string, pressure bool) {
	payload, _ := json.Marshal(map[string]any{"tool_name": "Agent", "tool_input": map[string]any{
		"subagent_type": in.Type, "model": in.Model, "prompt": in.Brief, "description": labelDescription}})
	ci := core.ParseInputFor(bytes.NewReader(payload), "") // a Claude spawn: gemini's parser would empty tool_input
	updated := ci.CloneToolInput()
	promptField, task := spawnTask(ci)
	if in.Brief != "" { // the hook classifies the brief; the stand-in description is not one
		pinModel(ci, updated, task)
	}
	before := maps.Clone(updated)
	(&effortPlan{updated: updated}).pressure(ci, promptField, task)
	pressure = before["model"] != updated["model"] || before["subagent_type"] != updated["subagent_type"]
	label, _ = leafLabel(updated, in.Type, task, labelDescription, func() string { return in.SessionModel })
	model, _ = updated["model"].(string)
	return label, model, pressure
}
