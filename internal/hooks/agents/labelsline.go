package agents

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/transcript"
)

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

// dispatchLabelsLine is the label table for the session's current model, "" when already shown for that model or the model is unknown.
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
	for _, t := range dispatchTypes() {
		label, _, blocked := dispatchLabel(context.Background(), LabelInput{Type: t, SessionModel: model})
		if blocked == "" && label != "" {
			parts = append(parts, t+"="+label+":")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	if os.MkdirAll(filepath.Dir(file), 0o755) == nil {
		_ = os.WriteFile(file, []byte(model), 0o644) // best effort: a failed write only re-shows the line next prompt
	}
	return "Dispatch labels (description prefix): " + strings.Join(parts, " · ")
}

// sessionModel is the last turn's model, else the SessionStart payload's own (the transcript is empty then).
func sessionModel(in *core.Input) string {
	if u, _ := transcript.LastUsage(in.TranscriptPath); u.Model != "" {
		return u.Model
	}
	return in.Nested("model")
}
