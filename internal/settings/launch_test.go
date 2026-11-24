package settings

import "testing"

func TestLaunchSettingsWebTavilyHookOnEveryLaunch(t *testing.T) {
	for _, databricks := range []bool{true, false} {
		doc, err := LaunchSettings(databricks)
		if err != nil {
			t.Fatal(err)
		}
		hooks, _ := doc["hooks"].(map[string]any)
		entries, _ := hooks[EventPostToolUse].([]any)
		if len(entries) != 1 || entries[0].(map[string]any)["matcher"] != "WebFetch|WebSearch" {
			t.Errorf("databricks=%v: PostToolUse entries = %v", databricks, entries)
		}
		pre, _ := hooks[EventPreToolUse].([]any)
		if len(pre) != 1 || pre[0].(map[string]any)["matcher"] != "MultiEdit|Write|Edit|NotebookEdit" {
			t.Errorf("databricks=%v: PreToolUse entries = %v, want the safety-claude-edit gate", databricks, pre)
		}
	}
}
