package hooks

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/transcript"
)

// briefAgents are the agents the `Agent:` line names, claude when the line is absent; nil for in-context.
func briefAgents(task string) []string {
	m := agentLineRe().FindStringSubmatch(briefHeader(task))
	if m == nil {
		return []string{"claude"}
	}
	line := strings.ToLower(m[1])
	if strings.Contains(line, "in-context") {
		return nil
	}
	var names []string
	for _, w := range agentNameRe().FindAllString(line, -1) {
		if labelAgent(w) && !slices.Contains(names, w) {
			names = append(names, w)
		}
	}
	return names
}

// briefHeader is the brief's header block: the lines before its first blank line, past an opening Run: line.
func briefHeader(task string) string {
	paras := blankLineRe().Split(strings.TrimSpace(task), 3)
	if len(paras) > 1 && strings.HasPrefix(paras[0], "Run:") && !strings.Contains(paras[0], "\n") {
		return paras[0] + "\n" + paras[1]
	}
	return paras[0]
}

// LabelInput is one dispatch to label: leaf type, model pin ("" = unset), ask --profile, brief and session model id.
type LabelInput struct {
	Type, Model, Profile, Brief, SessionModel string
	askModel                                  bool // Model is the ask's --model (the verb), not the Agent call's own
}

// dispatchLabel runs the hook's type gate, pin and label on a synthetic spawn; blocked is the type gate's deny.
func dispatchLabel(ctx context.Context, in LabelInput) (label, model, blocked string) {
	label, model, blocked, _ = dispatchLabelNote(ctx, in)
	return label, model, blocked
}

// dispatchLabelNote is dispatchLabel plus the label's note: the route's substitution, or why none resolved.
func dispatchLabelNote(ctx context.Context, in LabelInput) (label, model, blocked, note string) {
	if deny := typeBlock(in.Type, in.Brief); deny != nil {
		return "", "", deny.Reason, ""
	}
	payload, _ := json.Marshal(map[string]any{"tool_name": "Agent", "tool_input": map[string]any{
		"subagent_type": in.Type, "model": in.Model, "prompt": in.Brief}})
	ci := core.ParseInputFor(bytes.NewReader(payload), "") // a Claude spawn: gemini's parser would empty tool_input
	updated := ci.CloneToolInput()
	_, task := spawnTask(ci)
	run := newDispatchRun(func() string { return in.SessionModel })
	run.profile = in.Profile
	if in.askModel {
		run.model = in.Model
	}
	if in.Brief != "" { // the hook classifies the brief; the stand-in description is not one
		pinModel(run, ci, updated, task)
	}
	label, note = labelModel(ctx, run, updated, in.Type, task) // applyLabel's prefix, whatever the description
	model, _ = updated["model"].(string)
	return label, model, "", note
}

// LabelRow is `agents label`'s result: the leaf type, the model it carries, its prefix and where the model came from.
type LabelRow struct {
	Type   string `json:"type"`
	Model  string `json:"model"`
	Label  string `json:"label"`
	Source string `json:"source"`
	// Blocked is the hook's deny reason for a type it refuses ("" = allowed).
	Blocked string `json:"blocked,omitempty"`
	Note    string `json:"note,omitempty"` // the route's substitution, or why no label resolved offline
}

// Label labels in; an empty SessionModel is read from session sid's transcript in cwd's project.
func Label(in LabelInput, cwd, sid string) LabelRow {
	in.SessionModel = cmp.Or(in.SessionModel, sessionModelOf(cwd, sid))
	if in.Brief == "" && in.Model == "" && !agentPinsModel(newDispatchRun(func() string { return in.SessionModel }), in.Type) {
		if tiers := dispatchTierLabels(in.Type, in.SessionModel); tiers != "" { // the hook picks the tier from the brief
			return LabelRow{Type: in.Type, Label: tiers, Source: "derived"}
		}
	}
	in.askModel = true
	label, pinned, blocked, note := dispatchLabelNote(context.Background(), in)
	return LabelRow{Type: in.Type, Model: pinned, Label: label, Source: labelSource(in.Model, label, in.SessionModel), Blocked: blocked, Note: note}
}

// labelSource names where the label's model came from: the dispatch's --model, the session, or the pin and route.
func labelSource(dispatchModel, label, sessionModel string) string {
	switch {
	case dispatchModel != "" && dispatchModel != "inherit" && models.SameModel(label, dispatchModel):
		return "dispatch"
	case sessionModel != "" && models.SameModel(label, sessionModel):
		return "session"
	}
	return "derived"
}

// sessionModelOf is the model of the last turn in session sid's transcript in cwd's project; "" when unreadable.
func sessionModelOf(cwd, sid string) string {
	u, _ := transcript.LastUsage(memory.LiveTranscript(cwd, sid))
	return u.Model
}

// WithDispatchLabels appends the dispatch-label line to a Claude hook's context when the session model changed.
func WithDispatchLabels(next core.Runner) core.Runner {
	return func(in *core.Input) core.Result {
		res := next(in)
		if in.Agent() != core.AgentClaude {
			return res
		}
		res.Context = core.JoinContext(res.Context, dispatchLabelsLine(in))
		return res
	}
}

// dispatchLabelsLine reports labels once per known session model, types sharing a list grouped; "" otherwise.
func dispatchLabelsLine(in *core.Input) string {
	model := sessionModel(in)
	if model == "" {
		return ""
	}
	file := filepath.Join(paths.StateDir(), "dispatch-labels", paths.Slug(in.SessionID, "", 80, "session"))
	if prev, err := os.ReadFile(file); err == nil && string(prev) == model {
		return ""
	}
	var parts []string
	byLabels := map[string][]string{}
	run := newDispatchRun(func() string { return model })
	for _, t := range dispatchTypes() {
		if labels := dispatchLabelEntry(run, t, model); labels != "" {
			if byLabels[labels] == nil {
				parts = append(parts, labels)
			}
			byLabels[labels] = append(byLabels[labels], t)
		}
	}
	for i, labels := range parts {
		parts[i] = strings.Join(byLabels[labels], ",") + "=" + labels // `a,b=haiku:|sonnet:|opus:`
	}
	if len(parts) == 0 {
		return ""
	}
	if os.MkdirAll(filepath.Dir(file), 0o700) == nil {
		_ = os.WriteFile(file, []byte(model), 0o600) // best effort: a failed write only re-shows the line next prompt
	}
	return "Dispatch labels (description prefix): " + strings.Join(parts, " · ") + "\n" + labelRuleLine
}

// labelRuleLine is the one extra session-start line: how a Run line's flags change the label.
const labelRuleLine = "A Run line's --model/--profile makes the label that ask's model (databricks- id if routed); equivalents pass."

// dispatchLabelEntry is the type's accepted prefixes: its pin's label, else one per tier; "" when blocked.
func dispatchLabelEntry(run *dispatchRun, t, model string) string {
	if agentPinsModel(run, t) {
		label, _, blocked := dispatchLabel(context.Background(), LabelInput{Type: t, SessionModel: model})
		if blocked != "" || label == "" {
			return ""
		}
		return label + ":"
	}
	return dispatchTierLabels(t, model)
}

// dispatchTierLabels is the accepted prefix per tier (`haiku:|sonnet:|opus:`); "" when the type is blocked.
func dispatchTierLabels(t, model string) string {
	var labels []string
	for _, tier := range []string{models.Haiku, models.Sonnet, models.Opus} {
		label, _, blocked := dispatchLabel(context.Background(), LabelInput{Type: t, Model: tier, SessionModel: model})
		if blocked != "" || label == "" {
			return ""
		}
		if !slices.Contains(labels, label+":") {
			labels = append(labels, label+":")
		}
	}
	return strings.Join(labels, "|")
}

// sessionModel is the last turn's model, else the SessionStart payload's own (the transcript is empty then).
func sessionModel(in *core.Input) string {
	if u, _ := transcript.LastUsage(in.TranscriptPath); u.Model != "" {
		return u.Model
	}
	return in.Nested("model")
}
