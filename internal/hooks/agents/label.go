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

// dispatchLabel runs the hook's type gate, pin and label on a synthetic spawn; blocked is the type gate's deny.
func dispatchLabel(in LabelInput) (label, model, blocked string) {
	if deny := typeBlock(in.Type, in.Brief); deny != nil {
		return "", "", deny.Reason
	}
	payload, _ := json.Marshal(map[string]any{"tool_name": "Agent", "tool_input": map[string]any{
		"subagent_type": in.Type, "model": in.Model, "prompt": in.Brief}})
	ci := core.ParseInputFor(bytes.NewReader(payload), "") // a Claude spawn: gemini's parser would empty tool_input
	updated := ci.CloneToolInput()
	_, task := spawnTask(ci)
	run := newDispatchRun(func() string { return in.SessionModel })
	if in.Brief != "" { // the hook classifies the brief; the stand-in description is not one
		pinModel(run, ci, updated, task)
	}
	label = labelModel(run, updated, in.Type, task) // applyLabel's prefix, whatever the description
	model, _ = updated["model"].(string)
	return label, model, ""
}

// LabelRow is `agents label`'s result: the leaf type, the model it carries, its prefix and where the model came from.
type LabelRow struct {
	Type   string `json:"type"`
	Model  string `json:"model"`
	Label  string `json:"label"`
	Source string `json:"source"`
	// Blocked is the hook's deny reason for a type it refuses ("" = allowed).
	Blocked string `json:"blocked,omitempty"`
}

// Label labels in; an empty SessionModel is read from session sid's transcript in cwd's project.
func Label(in LabelInput, cwd, sid string) LabelRow {
	in.SessionModel = cmp.Or(in.SessionModel, sessionModelOf(cwd, sid))
	label, pinned, blocked := dispatchLabel(in)
	return LabelRow{Type: in.Type, Model: pinned, Label: label, Source: labelSource(in.Model, label, in.SessionModel), Blocked: blocked}
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
