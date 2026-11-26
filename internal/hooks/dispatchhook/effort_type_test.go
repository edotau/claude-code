package dispatchhook

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/platform/textutil"
	"github.com/edotau/claude-code/internal/routing/agentconfig/project"
)

// The roster is the agents/ files plus the built-ins (the `agents label` verb refuses others; the hook never does).
func TestDispatchTypeGateFollowsAgentFiles(t *testing.T) {
	cfg := useAgentsDir(t, "claude", "worker", "Plan")
	blocked := func(st, brief string) string {
		if r := typeBlock(st); r != nil && brief != "" {
			return r.Reason
		}
		return ""
	}
	for _, st := range []string{"claude", "worker", "Plan", "Explore", "general-purpose", "web-fetch", "claude-code-guide", "statusline-setup", ""} {
		if r := blocked(st, "do it"); r != "" {
			t.Errorf("%q blocked: %s", st, r)
		}
	}
	const roster = "(Explore, Plan, claude, claude-code-guide, general-purpose, statusline-setup, web-fetch, worker)"
	for st, c := range map[string]struct{ brief, use string }{
		"copilot": {"do it", `use "general-purpose"`}, "not-a-type": {"read a page", `use "general-purpose"`},
		"claude-context-optimizer:context-analyzer": {"q", `use "general-purpose"`},
		"coworker:copilot":                          {"do it", `use "copilot"`},
		"coworker:opencode":                         {"do it", `use "opencode"`},
		"coworker":                                  {"Agent: codex\n\ndo it", `use "general-purpose"`},
	} {
		want := fmt.Sprintf("dispatch-effort: subagent_type %q is not on the roster %s; %s", st, roster, c.use)
		if r := blocked(st, c.brief); r != want {
			t.Errorf("%s: reason %q, want %q", st, r, want)
		}
	}
	writeAgentFile(t, cfg, "helper")
	if r := blocked("helper", "do it"); r != "" {
		t.Errorf("helper with its file added still blocked: %s", r)
	}
	if err := os.Remove(filepath.Join(cfg, "agents", "worker.md")); err != nil {
		t.Fatal(err)
	}
	if r := blocked("worker", "do it"); !strings.Contains(r, "is not on the roster") {
		t.Errorf("worker with its file removed = %q, want blocked", r)
	}
	if err := os.Remove(filepath.Join(cfg, "agents", "Plan.md")); err != nil {
		t.Fatal(err)
	}
	if r := blocked("Plan", "do it"); !strings.Contains(r, `use "claude" and state read-only`) {
		t.Errorf("Plan with its file removed = %q, want blocked with the claude redirect", r)
	}
}

// An unreadable or empty agents dir never blocks: the roster falls back to agentid plus the built-ins.
func TestDispatchTypeGateFallsBackToRoster(t *testing.T) {
	orig := listAgents
	t.Cleanup(func() { listAgents = orig })
	listAgents = func(string) ([]project.Source, error) { return nil, os.ErrPermission }
	if got, want := dispatchTypes(), []string{"Explore", "claude", "claude-code-guide", "codex", "copilot", "coworker", "gemini", "general-purpose", "opencode", "statusline-setup", "web-fetch"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("fallback types = %v, want %v", got, want)
	}
	useAgentsDir(t) // no agents/ at all
	listAgents = orig
	if r := typeBlock("gemini"); r != nil {
		t.Errorf("gemini with no agents dir blocked: %s", r.Reason)
	}
}

// A type off the roster is left to the host: the hook neither blocks it nor claims a slot.
func TestDispatchUnknownTypeIsNotBlocked(t *testing.T) {
	dispatchEnv(t)
	writeAgentFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), "claude")
	writeAgentFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), "coworker")
	if r := pre(`{"tool_name":"Agent","tool_use_id":"gate1","tool_input":{"subagent_type":"not-a-type","prompt":"x","description":"x"}}`); r.Decision == core.Block {
		t.Fatalf("not-a-type = %+v, want it through", r)
	}
	if r := pre(`{"tool_name":"Agent","tool_use_id":"gate3","tool_input":{"subagent_type":"coworker:codex","prompt":"x","description":"x"}}`); r.Decision == core.Block {
		t.Fatalf("coworker:codex = %+v, want it through", r)
	}
	if r := pre(`{"tool_name":"Agent","tool_use_id":"gate2","tool_input":{"subagent_type":"claude","model":"opus","prompt":"x","description":"x"}}`); r.Decision == core.Block || !hasMarker(t, "gate2") {
		t.Fatalf("claude = %+v, want a claimed slot", r)
	}
}

// A retired name retyped to coworker pins model and effort exactly as a direct coworker dispatch.
func TestDispatchStaleRetypeMatchesDirect(t *testing.T) {
	dispatchEnv(t)
	cfg := os.Getenv("CLAUDE_CONFIG_DIR")
	if err := os.MkdirAll(filepath.Join(cfg, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: coworker\nmodel: haiku\neffort: low\n---\n"
	if err := os.WriteFile(filepath.Join(cfg, "agents", "coworker.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(st string) core.Result {
		return pre(fmt.Sprintf(`{"tool_name":"Agent","tool_input":{"subagent_type":%q,"prompt":"fix a typo","description":"fix a typo"}}`, st))
	}
	direct, retyped := run("coworker"), run("code-mode")
	if retyped.UpdatedInput["subagent_type"] != "coworker" {
		t.Fatalf("code-mode = %+v, want a retype to coworker", retyped)
	}
	for _, k := range []string{"model", "effort"} {
		if direct.UpdatedInput[k] != retyped.UpdatedInput[k] {
			t.Errorf("%s: retyped %v, direct %v, want equal (the agent file pins both)", k, retyped.UpdatedInput[k], direct.UpdatedInput[k])
		}
	}
	if retyped.UpdatedInput["model"] != nil || retyped.UpdatedInput["effort"] != nil {
		t.Errorf("retyped = %v, want no model or effort pin over the agent file", retyped.UpdatedInput)
	}
}

func TestDispatchCodemodeRetype(t *testing.T) {
	dispatchEnv(t)
	writeAgentFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), "claude")
	writeAgentFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), "coworker")
	for _, c := range []struct{ st, brief, want string }{
		{"coworker", "Agent: pi\\nCodemode: off\\n\\nbody", "claude"},
		{"claude", "Agent: pi\\nCodemode: on\\n\\nbody", "coworker"},
		{"coworker", "Agent: pi\\n\\nbody", "coworker"},
		{"pi", "Agent: pi\\n\\nbody", "coworker"},
		{"pi", "Agent: pi\\nCodemode: off\\n\\nbody", "claude"},
		{"code-mode", "body", "coworker"},
		{"claude", "Agent: pi\\nrun --codemode off\\n\\nbody", "coworker"}, // prose is not the Codemode: line
		{"claude", "Agent: code-mode\\n\\nbody", "coworker"},
		{"claude", "Agent: pi\\nCodemode: off\\n\\nbody", "claude"},
		{"claude", "Agent: codex\\n\\nbody", "claude"},
	} {
		r := pre(fmt.Sprintf(`{"tool_name":"Agent","tool_input":{"subagent_type":%q,"prompt":"%s","description":"x"}}`, c.st, c.brief))
		got, _ := r.UpdatedInput["subagent_type"].(string)
		if r.Decision == core.Block || got != c.want {
			t.Errorf("%s %q = %+v, want type %s", c.st, c.brief, r, c.want)
		}
	}
}

func TestDispatchCoworkerPiSuggestsCoworker(t *testing.T) {
	if got := typeSuggestion("coworker:pi"); got != `use "coworker"` {
		t.Errorf("suggestion = %q, want coworker", got)
	}
}

func TestDispatchCodeModeOffKeepsPiAgentLine(t *testing.T) {
	dispatchEnv(t)
	writeAgentFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), "claude")
	writeAgentFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), "coworker")
	r := pre(`{"tool_name":"Agent","tool_input":{"subagent_type":"coworker","prompt":"Codemode: off\n\nbody","description":"x"}}`)
	p, _ := r.UpdatedInput["prompt"].(string)
	if got, _ := r.UpdatedInput["subagent_type"].(string); got != "claude" || !strings.HasPrefix(p, "Agent: pi\n") {
		t.Errorf("type %q prompt %q, want claude with an Agent: pi header line", got, p)
	}
}

func TestAskLineRoutePicksAgentLineAcrossContinuations(t *testing.T) {
	text := "claude-code agents ask --agent claude --profile x\nclaude-code agents ask --background \\\n  --agent pi --provider databricks --profile qa - < PATH\n"
	if prov, prof := askLineRoute(text, "pi"); prov != "databricks" || prof != "qa" {
		t.Errorf("route = %q %q, want databricks qa", prov, prof)
	}
}

func TestAskLineRouteReadsOverlayAndFlags(t *testing.T) {
	cases := []struct{ name, text, prov, prof string }{
		{"flags", "claude-code agents ask --agent pi --provider databricks --profile qa -", "databricks", "qa"},
		{"overlay", `claude-code agents ask --carry --overlay '{"agent":"pi","provider":"databricks","profile":"qa","model":"m","tier":"fast"}' -`, "databricks", "qa"},
		{"flag over overlay", `claude-code agents ask --agent pi --overlay '{"agent":"codex","provider":"databricks","profile":"qa"}' -`, "databricks", "qa"},
		{"overlay other agent", `claude-code agents ask --overlay '{"agent":"codex","provider":"a","profile":"b"}' -`, "", ""},
		{"malformed overlay", `claude-code agents ask --agent pi --provider databricks --profile qa --overlay '{"agent":' -`, "", ""},
		{"non-string value", `claude-code agents ask --overlay '{"agent":"pi","provider":3}' -`, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if prov, prof := askLineRoute(c.text, "pi"); prov != c.prov || prof != c.prof {
				t.Errorf("route = %q %q, want %q %q", prov, prof, c.prov, c.prof)
			}
		})
	}
}

func TestReadAskLineFields(t *testing.T) {
	l, ok := readAskLine(quotedFields(`--overlay '{"agent":"pi","provider":"p","profile":"q","model":"m","tier":"t"}' -`))
	if want := (askLine{"pi", "p", "q", "m", "t"}); !ok || l != want {
		t.Errorf("line = %+v %v, want %+v", l, ok, want)
	}
}

func TestAskReadersFlagPrecedence(t *testing.T) {
	const overlay = `{"agent":"pi","provider":"databricks","profile":"qa","model":"sonnet","tier":"fast","timeout":"30s"}`
	for _, c := range []struct {
		name, flags string
		want        askLine
		timeout     string
	}{
		{"overlay", "", askLine{"pi", "databricks", "qa", "sonnet", "fast"}, "30s"},
		{"flags before", "--agent codex --model opus --tier deep --timeout 2m ", askLine{"codex", "databricks", "qa", "opus", "deep"}, "2m"},
		{"last flag", "--agent gemini --agent codex ", askLine{"codex", "databricks", "qa", "sonnet", "fast"}, "30s"},
		{"empty flag", "--model '' ", askLine{"pi", "databricks", "qa", "", "fast"}, "30s"},
		{"same route", "--provider databricks --profile qa ", askLine{"pi", "databricks", "qa", "sonnet", "fast"}, "30s"},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, line := range []string{c.flags + "--overlay '" + overlay + "' -", "--overlay '" + overlay + "' " + c.flags + "-"} {
				assertAskReaders(t, line, c.want, c.timeout)
			}
		})
	}
	assertAskReaders(t, `--agent codex --provider databricks --profile dev --timeout 1m --overlay '{"model":"opus"}' -`,
		askLine{"codex", "databricks", "dev", "opus", ""}, "1m")
	assertAskReaders(t, `--overlay '{"Agent":"codex","AGENT":"pi","Provider":"databricks","Profile":"qa","TIMEOUT":"2m"}' -`,
		askLine{"pi", "databricks", "qa", "", ""}, "2m")
	assertAskReaders(t, `--overlay '{"AGENT":"pi","agent":"codex","Provider":"databricks","Profile":"qa","Timeout":"2m"}' -`,
		askLine{"codex", "databricks", "qa", "", ""}, "2m")
}

func assertAskReaders(t *testing.T, line string, want askLine, timeout string) {
	t.Helper()
	a, ok := ParseAskWords(relayWords(line))
	if got := (askLine{a.Value("agent"), a.Value("provider"), a.Value("profile"), a.Value("model"), a.Value("tier")}); !ok || got != want || a.Value("timeout") != timeout {
		t.Errorf("receipt reader %q = %+v, timeout %q, ok %v; want %+v, timeout %q", line, got, a.Value("timeout"), ok, want, timeout)
	}
	if got, ok := readAskLine(quotedFields(line)); !ok || got != want {
		t.Errorf("label reader %q = %+v, ok %v; want %+v", line, got, ok, want)
	}
}

func TestAskLineUnreadableOverlay(t *testing.T) {
	for _, value := range []string{
		"file.json", "-", "", "null", "[]", `{"agent":`, `{"agent":"pi"} {}`, `{"agent":null}`, `{"timeout":3}`,
		`{"model":3,"model":"x"}`, `{"MODEL":3,"model":"x"}`, `{"agent":"pi","Agent":null}`,
	} {
		line := `--agent pi --provider databricks --profile qa --overlay ` + textutil.ShellQuote(value) + ` -`
		if got, ok := readAskLine(quotedFields(line)); ok {
			t.Errorf("%q read as %+v, want unreadable", line, got)
		}
	}
}

func TestAskReadersShellQuoting(t *testing.T) {
	for _, model := range []string{`a'b`, `a"b`, `a\b`, `a b`, `a'b"c\d`, `a$(exit 1);b`} {
		raw, err := json.Marshal(map[string]string{"agent": "pi", "model": model, "timeout": "2m"})
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range []string{
			textutil.ShellJoin([]string{"--overlay", string(raw)}),
			textutil.ShellQuote("--overlay") + " " + textutil.ShellQuote(string(raw)),
			"--overlay=" + textutil.ShellQuote(string(raw)),
			"--overlay " + `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`").Replace(string(raw)) + `"`,
		} {
			assertAskReaders(t, line, askLine{Agent: "pi", Model: model}, "2m")
		}
	}
	for _, line := range []string{`--overlay '{"agent":"pi"}`, `--agent pi\`} {
		if got := quotedFields(line); got != nil {
			t.Errorf("unterminated %q = %q", line, got)
		}
	}
}

func TestAskLineTrailingComments(t *testing.T) {
	for _, tail := range []string{" # don't", ` # "unfinished`, ` # trailing\`, " # --agent codex"} {
		line := `claude-code agents ask --overlay '{"agent":"pi","provider":"databricks","profile":"qa"}' -` + tail
		if provider, profile := askLineRoute(line, "pi"); provider != "databricks" || profile != "qa" {
			t.Errorf("%q: route = %q/%q, want databricks/qa", line, provider, profile)
		}
	}
	line := `claude-code agents ask --overlay '{"agent":"pi","model":"a#b"}' - # don't`
	if got, ok := readAskLine(quotedFields(strings.TrimPrefix(line, "claude-code agents ask "))); !ok || got.Model != "a#b" {
		t.Errorf("quoted hash: %+v, %v; want model a#b", got, ok)
	}
}
