package hookspec

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func TestCodexNativeRoutingHooksStayVendorScoped(t *testing.T) {
	codex, err := CodexJSON()
	if err != nil {
		t.Fatal(err)
	}
	claude, err := SettingsJSON()
	if err != nil {
		t.Fatal(err)
	}
	copilot, err := CopilotJSON()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks map[string][]Group `json:"hooks"`
	}
	if err := json.Unmarshal(codex, &doc); err != nil {
		t.Fatal(err)
	}
	for name, event := range map[string]string{
		"codex-agent-route":    EventPreToolUse,
		"codex-routing-result": EventPostToolUse,
		"codex-routing-start":  EventSessionStart,
		"codex-subagent-start": EventSubagentStart,
	} {
		command := AgentEnv + "=codex " + HookBin + " hook " + name
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
					if event == EventPreToolUse || event == EventPostToolUse {
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
			if strings.Contains(string(body), "hook "+name) {
				t.Errorf("%s hook leaked into %s", name, vendor)
			}
		}
	}
}
