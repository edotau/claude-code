package roles

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent"
)

// MaxParts caps the explore sub-questions (split or --part) and the legs running at once.
const MaxParts = 12

// listMarker is a leading bullet or number the model adds despite the prompt.
var listMarker = regexp.MustCompile(`^(?:[-*•]|\d+[.)])\s+`)

// splitRun is the model-call seam for Split.
var splitRun = (*agent.Client).Run

// Split asks a cheap model for independent sub-questions; any error or a single line returns question whole.
func Split(ctx context.Context, c *agent.Client, question string) []string {
	res, err := splitRun(c, ctx, agent.RunOptions{
		Agent: "claude", Model: "haiku", NoTools: true, Timeout: 60 * time.Second,
		Prompt: "Split into 2–4 independent sub-questions, one per line, no numbering:\n" + question,
	}, wire.Discard)
	if err != nil {
		return []string{question}
	}
	var parts []string
	for l := range strings.SplitSeq(res.Answer, "\n") {
		if l = listMarker.ReplaceAllString(strings.TrimSpace(l), ""); l != "" && len(parts) < MaxParts {
			parts = append(parts, l)
		}
	}
	if len(parts) <= 1 {
		return []string{question}
	}
	return parts
}
