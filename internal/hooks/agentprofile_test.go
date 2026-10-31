package hooks

import (
	"encoding/json"
	"strings"
	"testing"
)

func stubProfiles(t *testing.T, names []string, ok bool) {
	t.Helper()
	old := profileNames
	profileNames = func() ([]string, bool) { return names, ok }
	t.Cleanup(func() { profileNames = old })
}

func profileSpawn(t *testing.T, kind, prompt string) string {
	t.Helper()
	in := map[string]any{"subagent_type": kind, "prompt": prompt, "description": "fix thing"}
	b, err := json.Marshal(map[string]any{"tool_name": "Agent", "session_id": "s", "tool_input": in})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Any non-coworker type with a Profile: line moves to coworker with the exact ask command prepended, once.
func TestProfileBriefReroutesToCoworkerIdempotently(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	stubProfiles(t, []string{"dbx", "openai"}, true)
	for _, kind := range []string{"general-purpose", "code-worker", "coworker"} {
		brief := "Agent: codex\nProfile: dbx\nreview a.go"
		res := agentEffortResult(ParseInput(strings.NewReader(profileSpawn(t, kind, brief))))
		if res.Decision == Block || res.UpdatedInput == nil {
			t.Fatalf("%s: res = %+v, want an update", kind, res)
		}
		if got := res.UpdatedInput["subagent_type"]; got != "coworker" {
			t.Errorf("%s: subagent_type = %v, want coworker", kind, got)
		}
		p, _ := res.UpdatedInput["prompt"].(string)
		if !strings.HasPrefix(p, "Run: claude-code ask --agent codex --profile dbx") || !strings.HasSuffix(p, brief) {
			t.Errorf("%s: prompt = %q, want the Run: line then the brief", kind, p)
		}
		again := agentEffortResult(ParseInput(strings.NewReader(profileSpawn(t, "coworker", p))))
		if q, _ := again.UpdatedInput["prompt"].(string); again.UpdatedInput != nil && q != p {
			t.Errorf("%s: second pass changed the prompt:\n%q\nvs\n%q", kind, q, p)
		}
	}
}

func TestProfileBriefDefaultsToClaudeAsk(t *testing.T) {
	updated := map[string]any{"subagent_type": "general-purpose", "prompt": "Profile: dbx\ndo it"}
	stubProfiles(t, []string{"dbx"}, true)
	note, deny := applyProfile(updated, "prompt", "Profile: dbx\ndo it")
	if deny != nil || !strings.Contains(note, "moved to coworker") || !strings.HasPrefix(updated["prompt"].(string), "Run: claude-code ask --agent claude --profile dbx") {
		t.Fatalf("note=%q deny=%v updated=%v", note, deny, updated)
	}
}

// An unknown profile is a Block that lists every valid name; an unreadable registry fails open.
func TestProfileUnknownDeniedAndRegistryFailureOpen(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	brief := "Profile: nosuch\ndo it"
	stubProfiles(t, []string{"dbx", "openai"}, true)
	res := agentEffortResult(ParseInput(strings.NewReader(profileSpawn(t, "general-purpose", brief))))
	if res.Decision != Block || !strings.Contains(res.Reason, `"nosuch"`) || !strings.Contains(res.Reason, "dbx, openai") {
		t.Fatalf("unknown profile: res = %+v, want a Block naming nosuch and listing dbx, openai", res)
	}
	stubProfiles(t, nil, false)
	res = agentEffortResult(ParseInput(strings.NewReader(profileSpawn(t, "general-purpose", brief))))
	if res.Decision == Block || res.UpdatedInput["subagent_type"] != "coworker" {
		t.Fatalf("registry unreadable: res = %+v, want fail open and a coworker reroute", res)
	}
}

func TestProfileLabels(t *testing.T) {
	cases := []struct{ name, task, want string }{
		{"single", "Agent: codex\nProfile: dbx", "codex·dbx"},
		{"relay", "Agent: codex → copilot\nProfile: dbx", "codex → copilot·dbx"},
		{"compare", "Agent: codex, copilot\nMode: compare\nProfile: dbx", "codex, copilot·dbx"},
		{"default claude", "Profile: dbx\ndo it", "claude·dbx"},
		{"no profile", "Agent: codex", "codex"},
	}
	for _, c := range cases {
		if got := agentLabel("coworker", c.task); got != c.want {
			t.Errorf("%s: agentLabel = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestProfileSuffixNeverDoubles(t *testing.T) {
	task := "Profile: dbx"
	if got := profileSuffix("codex", task); got != "codex·dbx" {
		t.Errorf("profileSuffix = %q, want codex·dbx", got)
	}
	if got := profileSuffix("codex·dbx", task); got != "codex·dbx" {
		t.Errorf("already suffixed: %q gained a second profile", got)
	}
	if got := profileSuffix("", task); got != "" {
		t.Errorf("empty label became %q", got)
	}
	if got := profileSuffix("codex", "Agent: codex"); got != "codex" {
		t.Errorf("no Profile: line changed the label to %q", got)
	}
}

// A Profile: brief opts out of pressure steering: the profile note appears, the Claude-window note does not.
func TestProfileSkipsPressure(t *testing.T) {
	withPressure(t, 99)
	t.Setenv("HARNESS_ROUTE_TEST_ANTHROPIC_PCT", "99")
	stubProfiles(t, []string{"dbx"}, true)
	control := runAgentEffortFull(t, profileSpawn(t, "code-worker", "do it"))
	if !strings.Contains(control.HSO.AdditionalContext, "window at") {
		t.Fatalf("control without a profile has no pressure note (%q); the test cannot bite", control.HSO.AdditionalContext)
	}
	doc := runAgentEffortFull(t, profileSpawn(t, "code-worker", "Profile: dbx\ndo it"))
	ctx := doc.HSO.AdditionalContext
	if !strings.Contains(ctx, "Profile: dbx") || strings.Contains(ctx, "window at") {
		t.Errorf("context = %q, want the profile note and no pressure note", ctx)
	}
	if doc.HSO.UpdatedInput["subagent_type"] != "coworker" || doc.HSO.UpdatedInput["model"] == "haiku" {
		t.Errorf("updated = %+v, want coworker not forced to the pressure haiku", doc.HSO.UpdatedInput)
	}
}

// Read-only types and in-context briefs keep their type and get no ask command.
func TestProfileIgnoredForReadOnlyAndInContext(t *testing.T) {
	stubProfiles(t, []string{"dbx"}, true)
	for kind, brief := range map[string]string{
		"Plan": "Profile: dbx\nplan it", "Explore": "Agent: codex\nProfile: dbx\nlook", "gemini": "Profile: dbx\nlook",
		"claude-code-guide": "Profile: dbx\nask", "general-purpose": "Agent: in-context\nProfile: dbx\ndo it",
	} {
		updated := map[string]any{"subagent_type": kind, "prompt": brief}
		note, deny := applyProfile(updated, "prompt", brief)
		if deny != nil || !strings.Contains(note, "ignored") || updated["subagent_type"] != kind || updated["prompt"] != brief {
			t.Errorf("%s: note=%q deny=%v updated=%v, want type and prompt untouched", kind, note, deny, updated)
		}
	}
}
