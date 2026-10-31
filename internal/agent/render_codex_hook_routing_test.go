package agent

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/settings"
)

func TestCodexNativeRoutingHooksStayVendorScoped(t *testing.T) {
	codex, err := CodexJSON()
	if err != nil {
		t.Fatal(err)
	}
	claude, err := settings.SettingsJSON()
	if err != nil {
		t.Fatal(err)
	}
	copilot, err := CopilotJSON()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks map[string][]settings.MatcherGroup `json:"hooks"`
	}
	if err := json.Unmarshal(codex, &doc); err != nil {
		t.Fatal(err)
	}
	for name, event := range map[string]string{
		"codex-agent-route":    settings.EventPreToolUse,
		"codex-routing-result": settings.EventPostToolUse,
		"codex-routing-start":  settings.EventSessionStart,
		"codex-subagent-start": settings.EventSubagentStart,
	} {
		command := settings.AgentEnv + "=codex " + settings.HookBin + " hook " + event + " --" + name
		found := 0
		for actualEvent, groups := range doc.Hooks {
			for _, group := range groups {
				for _, hook := range group.Hooks {
					if hook.Command != command {
						continue
					}
					found++
					if actualEvent != event || hook.Timeout != 5 || hook.Async {
						t.Errorf("%s rendered under %s with handler %+v", name, actualEvent, hook)
					}
					if event == settings.EventPreToolUse || event == settings.EventPostToolUse {
						matcher, err := regexp.Compile(group.Matcher)
						if err != nil {
							t.Fatal(err)
						}
						for _, tool := range []string{"spawn_agent", "collaborationspawn_agent", "multi_agent_v1.spawn_agent"} {
							if !matcher.MatchString(tool) {
								t.Errorf("%s matcher misses %q", name, tool)
							}
						}
						if matcher.MatchString("spawn_agent_extra") {
							t.Errorf("%s matches unrelated tool", name)
						}
					} else if group.Matcher != "" {
						t.Errorf("%s lifecycle matcher = %q", name, group.Matcher)
					}
				}
			}
		}
		if found != 1 {
			t.Errorf("%s rendered %d times", name, found)
		}
		for vendor, body := range map[string][]byte{"claude": claude, "copilot": copilot} {
			if strings.Contains(string(body), "--"+name) {
				t.Errorf("%s hook leaked into %s", name, vendor)
			}
		}
	}
}

func TestCodexJSONVendorMarker(t *testing.T) {
	b, err := CodexJSON()
	if err != nil {
		t.Fatal(err)
	}
	var block struct {
		Hooks map[string][]settings.MatcherGroup `json:"hooks"`
	}
	if err := json.Unmarshal(b, &block); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, groups := range block.Hooks {
		for _, group := range groups {
			for _, hook := range group.Hooks {
				count++
				if !strings.HasPrefix(hook.Command, "HARNESS_HOOK_AGENT=codex $HOME/.claude/bin/claude-code hook ") {
					t.Errorf("Codex command missing vendor marker: %q", hook.Command)
				}
				if !settings.IsHarness(hook.Command) {
					t.Errorf("Codex command not recognized as harness: %q", hook.Command)
				}
			}
		}
	}
	if count == 0 {
		t.Fatal("no Codex commands rendered")
	}
}
