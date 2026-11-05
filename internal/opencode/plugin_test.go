package opencode

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
)

// TestOpencodeApplyPluginRendersGuards writes the plugin into a hermetic config dir and wires it into the config.
func TestOpencodeApplyPluginRendersGuards(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	env, plugged, err := opencodeApplyPlugin([]string{opencodeConfigEnv + `={"plugin":["mine"]}`}, agent.RunOptions{Tools: true})
	var cfg struct{ Plugin []string }
	if err != nil || !plugged || json.Unmarshal([]byte(strings.TrimPrefix(env[0], opencodeConfigEnv+"=")), &cfg) != nil || len(cfg.Plugin) != 2 {
		t.Fatalf("env %v plugged %v err %v: want the user's plugin kept and ours appended", env, plugged, err)
	}
	file := strings.TrimPrefix(cfg.Plugin[1], "file://")
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("plugin file %s: %v, mode %v; want 0600", file, err, info)
	}
	body, _ := os.ReadFile(file)
	for _, want := range []string{`execFileSync(BIN, ["hook", "PreToolUse", "--" + m.name]`,
		`{"name":"permission-gate","matcher":"Bash|Write|Edit|Read|WebFetch","budgetMs":5000,"failClosed":true}`,
		`{"name":"docs-placement","matcher":"Write|Edit","budgetMs":5000,"failClosed":false}`, `"tool.execute.before"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("plugin lacks %s", want)
		}
	}
	if _, plugged, _ := opencodeApplyPlugin(nil, agent.RunOptions{Tools: true, SafeMode: true}); plugged {
		t.Error("--safe-mode (opencode --pure) loads no plugin, so none may be injected")
	}
}
