package tree

import (
	"strings"
	"testing"
	"time"
)

// The first column is the model's agent type, "<type> via pi" when pi carried it; provider and profile line up.
func TestJobRowsTypeLabelsAndAlignedRoute(t *testing.T) {
	now := time.Now()
	run := func(agent, model, kind, profile string) Dispatch {
		return Dispatch{ID: model, Agent: agent, Model: model, Status: statusRunning, Duration: "2s",
			launch: profile, Provider: "databricks", routeKind: kind}
	}
	for _, c := range []struct {
		d    Dispatch
		want string
	}{
		{run("claude", "databricks-claude-sonnet-5-5", "databricks", "qa"), "claude"},
		{run("codex", "databricks-gpt-6-sol", "databricks", "qa"), "codex"},
		{run("gemini", "gemini-4-argon", "databricks", "qa"), "gemini"},
		{run("copilot", "gpt-6-sol", "github", "qa"), "copilot"},
		{run("pi", "databricks-gpt-6-sol", "databricks", "dev"), "codex via pi"},
		{run("pi", "gemini-4-argon", "databricks", "dev"), "gemini via pi"},
		{run("pi", "gpt-6-sol", "github", "dev"), "copilot via pi"},
		{run("pi", "databricks-kimi-k3", "databricks", "dev"), "kimi via pi"},
		{run("pi", "databricks-grok-4-7", "databricks", "dev"), "grok via pi"},
	} {
		if got := jobCells(c.d, now, nil)[0]; got != c.want {
			t.Errorf("%s on %s: label %q, want %q", c.d.Agent, c.d.Model, got, c.want)
		}
	}
	ds := []Dispatch{run("codex", "databricks-gpt-6-sol", "databricks", "qa"), run("pi", "databricks-kimi-k3", "databricks", "dev")}
	var b strings.Builder
	renderTree(&b, &Tree{Dispatches: ds}, now, 0, "")
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")[1:]
	if strings.Index(lines[0], "· databricks") != strings.Index(lines[1], "· databricks") ||
		strings.Index(lines[0], "qa") != strings.Index(lines[1], "dev") {
		t.Errorf("route columns not aligned:\n%s", strings.Join(lines, "\n"))
	}
}
