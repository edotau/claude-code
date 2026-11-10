package agents

import (
	"bytes"
	"cmp"
	"encoding/json"
	"io"
	"os"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/transcript"
)

// LabelInput is one dispatch to label: its leaf type, model pin ("" = unset), brief and the session's model id.
type LabelInput struct {
	Type, Model, Brief, SessionModel string
}

// labelDescription stands in for the description: leafLabel needs a non-empty one, the prefix does not depend on it.
const labelDescription = "task"

// dispatchLabel runs the dispatch-effort pin and label path on a synthetic spawn: the prefix and the model the dispatch
// carries after the pin.
func dispatchLabel(in LabelInput) (label, model string) {
	payload, _ := json.Marshal(map[string]any{"tool_name": "Agent", "tool_input": map[string]any{
		"subagent_type": in.Type, "model": in.Model, "prompt": in.Brief, "description": labelDescription}})
	ci := core.ParseInputFor(bytes.NewReader(payload), "") // a Claude spawn: gemini's parser would empty tool_input
	updated := ci.CloneToolInput()
	_, task := spawnTask(ci)
	run := newDispatchRun(func() string { return in.SessionModel })
	if in.Brief != "" { // the hook classifies the brief; the stand-in description is not one
		pinModel(run, ci, updated, task)
	}
	label, _ = leafLabel(run, updated, in.Type, task, labelDescription)
	model, _ = updated["model"].(string)
	return label, model
}

// LabelRow is `agents label`'s result: the leaf type, the model it carries, its prefix and where the model came from.
type LabelRow struct {
	Type   string `json:"type"`
	Model  string `json:"model"`
	Label  string `json:"label"`
	Source string `json:"source"`
}

// Label labels in; an empty SessionModel is read from session sid's transcript in cwd's project.
func Label(in LabelInput, cwd, sid string) LabelRow {
	in.SessionModel = cmp.Or(in.SessionModel, sessionModelOf(cwd, sid))
	label, pinned := dispatchLabel(in)
	return LabelRow{Type: in.Type, Model: pinned, Label: label, Source: labelSource(in.Model, label, in.SessionModel)}
}

// labelSource names where the label's model came from: the dispatch's --model, the session, or the pin and route.
func labelSource(dispatchModel, label, sessionModel string) string {
	switch {
	case dispatchModel != "" && dispatchModel != "inherit" && label == dispatchModel:
		return "dispatch"
	case sessionModel != "" && label == sessionModel:
		return "session"
	}
	return "derived"
}

// sessionModelOf is the model of the last turn in session sid's transcript in cwd's project; "" when unreadable.
func sessionModelOf(cwd, sid string) string {
	u, _ := transcript.LastUsage(memory.LiveTranscript(cwd, sid))
	return u.Model
}

// ReadBrief returns the brief file's text; "" is an empty brief and "-" reads stdin.
func ReadBrief(path string) (string, error) {
	switch path {
	case "":
		return "", nil
	case "-":
		b, err := io.ReadAll(os.Stdin)
		return string(b), err
	}
	b, err := os.ReadFile(path)
	return string(b), err
}
