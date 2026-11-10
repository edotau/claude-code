package agents

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

func stubProfiles(t *testing.T, names []string, ok bool) {
	t.Helper()
	old := profileNames
	profileNames = func(*dispatchRun) ([]string, bool) { return names, ok }
	oldRow := servingRow
	servingRow = func(string) error { return nil }
	t.Cleanup(func() { profileNames, servingRow = old, oldRow })
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

// Any type with a Profile: line keeps its type and gets the exact ask command prepended, once.
func TestProfileBriefKeepsTypeIdempotently(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	stubProfiles(t, []string{"dbx", "openai"}, true)
	for _, kind := range []string{"general-purpose", "claude", "copilot"} {
		brief := "Agent: codex\nProfile: dbx\nreview a.go"
		res := agentEffortResult(core.ParseInput(strings.NewReader(profileSpawn(t, kind, brief))))
		if res.Decision == core.Block || res.UpdatedInput == nil {
			t.Fatalf("%s: res = %+v, want an update", kind, res)
		}
		if got := res.UpdatedInput["subagent_type"]; got != kind {
			t.Errorf("%s: subagent_type = %v, want the type kept", kind, got)
		}
		p, _ := res.UpdatedInput["prompt"].(string)
		if !strings.HasPrefix(p, "Run: claude-code agents ask --agent codex --profile dbx") || !strings.HasSuffix(p, brief) {
			t.Errorf("%s: prompt = %q, want the Run: line then the brief", kind, p)
		}
		again := agentEffortResult(core.ParseInput(strings.NewReader(profileSpawn(t, "claude", p))))
		if q, _ := again.UpdatedInput["prompt"].(string); again.UpdatedInput != nil && q != p {
			t.Errorf("%s: second pass changed the prompt:\n%q\nvs\n%q", kind, q, p)
		}
	}
}

func TestProfileBriefDefaultsToClaudeAsk(t *testing.T) {
	updated := map[string]any{"subagent_type": "general-purpose", "prompt": "Profile: dbx\ndo it"}
	stubProfiles(t, []string{"dbx"}, true)
	note, deny := applyProfile(newDispatchRun(nil), updated, "prompt", "Profile: dbx\ndo it")
	if deny != nil || strings.Contains(note, "moved to") || updated["subagent_type"] != "general-purpose" || !strings.HasPrefix(updated["prompt"].(string), "Run: claude-code agents ask --agent claude --profile dbx") {
		t.Fatalf("note=%q deny=%v updated=%v", note, deny, updated)
	}
}

// A sentence-ending period after the name ("Profile: dbx.") still routes to dbx, not an unknown "dbx.".
func TestProfileTrailingPeriodIsNotPartOfName(t *testing.T) {
	stubProfiles(t, []string{"dbx"}, true)
	updated := map[string]any{"subagent_type": "claude", "prompt": "Profile: dbx.\ndo it"}
	if note, deny := applyProfile(newDispatchRun(nil), updated, "prompt", "Profile: dbx.\ndo it"); deny != nil || !strings.Contains(note, "--profile dbx`") {
		t.Fatalf("note=%q deny=%v", note, deny)
	}
}

// An unknown profile is a Block that lists every valid name; an unreadable registry fails open.
func TestProfileUnknownDeniedAndRegistryFailureOpen(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	brief := "Profile: nosuch\ndo it"
	stubProfiles(t, []string{"dbx", "openai"}, true)
	res := agentEffortResult(core.ParseInput(strings.NewReader(profileSpawn(t, "general-purpose", brief))))
	if res.Decision != core.Block || !strings.Contains(res.Reason, `"nosuch"`) || !strings.Contains(res.Reason, "dbx, openai") {
		t.Fatalf("unknown profile: res = %+v, want a Block naming nosuch and listing dbx, openai", res)
	}
	stubProfiles(t, nil, false)
	res = agentEffortResult(core.ParseInput(strings.NewReader(profileSpawn(t, "general-purpose", brief))))
	if res.Decision == core.Block || res.UpdatedInput["subagent_type"] != "general-purpose" || !strings.HasPrefix(res.UpdatedInput["prompt"].(string), "Run: ") {
		t.Fatalf("registry unreadable: res = %+v, want fail open and the ask command on the kept type", res)
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
		if got := agentLabel("general-purpose", c.task); got != c.want {
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

// Read-only types and in-context briefs keep their type and get no ask command.
func TestProfileIgnoredForReadOnlyAndInContext(t *testing.T) {
	stubProfiles(t, []string{"dbx"}, true)
	for kind, brief := range map[string]string{
		"Plan": "Profile: dbx\nplan it", "Explore": "Agent: codex\nProfile: dbx\nlook", "gemini": "Profile: dbx\nlook",
		"general-purpose": "Agent: in-context\nProfile: dbx\ndo it",
	} {
		updated := map[string]any{"subagent_type": kind, "prompt": brief}
		note, deny := applyProfile(newDispatchRun(nil), updated, "prompt", brief)
		if deny != nil || !strings.Contains(note, "ignored") || updated["subagent_type"] != kind || updated["prompt"] != brief {
			t.Errorf("%s: note=%q deny=%v updated=%v, want type and prompt untouched", kind, note, deny, updated)
		}
	}
}

// TestArmProfileLineBlocked: under databricks:dev a Profile: line for another profile is blocked, dev's own passes.
func TestArmProfileLineBlocked(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_ROUTE", "databricks:dev")
	stubProfiles(t, []string{"dev", "qa"}, true)
	res := agentEffortResult(core.ParseInput(strings.NewReader(profileSpawn(t, "claude", "Agent: codex\nProfile: qa\nfix"))))
	if res.Decision != core.Block || !strings.Contains(res.Reason, "agent-effort: Profile: qa conflicts with databricks:dev") {
		t.Errorf("Profile: qa = %+v, want a Block naming databricks:dev", res)
	}
	res = agentEffortResult(core.ParseInput(strings.NewReader(profileSpawn(t, "claude", "Agent: codex\nProfile: dev\nfix"))))
	if p, _ := res.UpdatedInput["prompt"].(string); res.Decision == core.Block || !strings.HasPrefix(p, "Run: claude-code agents ask --agent codex --profile dev") {
		t.Errorf("Profile: dev = %+v, want the usual Run: rewrite", res)
	}
}

// routeSpawn runs one dispatch of type kind with brief prompt and returns the result.
func routeSpawn(t *testing.T, kind, prompt string) core.Result {
	t.Helper()
	return agentEffortResult(core.ParseInput(strings.NewReader(profileSpawn(t, kind, prompt))))
}

// A Route: line makes each external roster leaf run `agents ask --route`, bare Route: takes the session's profile.
func TestRouteLineRewritesExternalTypes(t *testing.T) {
	useAgentsDir(t, "claude", "codex", "copilot", "gemini", "opencode")
	stubProfiles(t, []string{"dbx", "qa"}, true)
	stubLabelModels(t, "", "")
	t.Setenv(providers.EnvSessionProfile, "")
	for _, kind := range []string{"codex", "copilot", "gemini", "opencode", "claude"} {
		res := routeSpawn(t, kind, "Route: databricks:dbx\nreview a.go")
		prompt, _ := res.UpdatedInput["prompt"].(string)
		if want := "Run: claude-code agents ask --agent " + kind + " --route databricks:dbx "; res.Decision == core.Block || !strings.HasPrefix(prompt, want) {
			t.Errorf("%s: prompt %q (reason %q), want it to start %q", kind, prompt, res.Reason, want)
		}
	}
	if r := routeSpawn(t, "codex", "Route: databricks\nx"); r.Decision != core.Block || !strings.Contains(r.Reason, "needs a workspace") {
		t.Errorf("bare Route with no session profile = %+v, want a block", r)
	}
	t.Setenv(providers.EnvSessionProfile, "qa")
	if p, _ := routeSpawn(t, "codex", "Route: databricks\nx").UpdatedInput["prompt"].(string); !strings.Contains(p, "--route databricks:qa ") {
		t.Errorf("bare Route with session profile qa: prompt %q", p)
	}
	if p, _ := routeSpawn(t, "Explore", "Route: databricks:dbx\nx").UpdatedInput["prompt"].(string); strings.Contains(p, "--route") {
		t.Errorf("Explore must ignore Route:, prompt %q", p)
	}
}

// Route: blocks an unknown profile, a different Profile:, and leaving an armed session; the armed one is a no-op.
func TestRouteLineConflicts(t *testing.T) {
	useAgentsDir(t, "codex")
	stubProfiles(t, []string{"dbx", "qa"}, true)
	stubLabelModels(t, "", "")
	for brief, want := range map[string]string{
		"Route: databricks:nope\nx":             "--route databricks:nope: no such profile",
		"Route: databricks:dbx\nProfile: qa\nx": "Route: databricks:dbx conflicts with Profile: qa",
		"Route: first-party\nx":                 "only databricks",
	} {
		if r := routeSpawn(t, "codex", brief); r.Decision != core.Block || !strings.Contains(r.Reason, want) {
			t.Errorf("%q = %+v, want a block naming %q", brief, r, want)
		}
	}
	if r := routeSpawn(t, "codex", "Route: databricks:dbx\nProfile: dbx\nx"); r.Decision == core.Block {
		t.Errorf("matching Route and Profile blocked: %s", r.Reason)
	}
	servingRow = func(p string) error {
		return errors.New("--route databricks:" + p + ": AI Gateway row; use a serving row")
	}
	if r := routeSpawn(t, "codex", "Route: databricks:dbx\nx"); r.Decision != core.Block || !strings.Contains(r.Reason, "AI Gateway row") {
		t.Errorf("a non-serving row = %+v, want a block naming the row kind", r)
	}
	servingRow = func(string) error { return nil }
	t.Setenv(router.EnvRoute, "databricks:qa")
	if r := routeSpawn(t, "codex", "Route: databricks:dbx\nx"); r.Decision != core.Block || !strings.Contains(r.Reason, "conflicts with databricks:qa") {
		t.Errorf("leaving the armed route = %+v, want a block", r)
	}
	if p, _ := routeSpawn(t, "codex", "Route: databricks:qa\nx").UpdatedInput["prompt"].(string); strings.Contains(p, "--route") {
		t.Errorf("Route matching the armed session must be a no-op, prompt %q", p)
	}
}

// A brief-level Route: labels with the same pick an armed session would.
func TestRouteLineLabelIsArmedPick(t *testing.T) {
	useAgentsDir(t, "codex")
	stubProfiles(t, []string{"dbx"}, true)
	stubLabelModels(t, "", "")
	labelCell = func(_ *dispatchRun, agent, alias string) string {
		return agent + "-" + alias + "@" + os.Getenv(router.EnvRoute)
	}
	label := func(brief string) string {
		d, _ := routeSpawn(t, "codex", brief).UpdatedInput["description"].(string)
		return d
	}
	brief := label("Route: databricks:dbx\nx")
	t.Setenv(router.EnvRoute, "databricks:dbx")
	if armed := label("x"); brief != armed || !strings.Contains(brief, "@databricks:dbx: fix thing") {
		t.Errorf("brief Route label %q, armed label %q, want the same pick inside databricks:dbx", brief, armed)
	}
}
