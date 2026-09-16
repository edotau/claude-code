package settings

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hookspec"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestRenderGolden(t *testing.T) {
	got, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "settings.golden.json")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("render drifted from golden (go test -run TestRenderGolden -update):\n%s", got)
	}
	if !json.Valid(got) {
		t.Error("render is not valid JSON")
	}
}

const existing = `{
  "env": {"KEEP_ME": "1"},
  "theme": "dark",
  "permissions": {"allow": ["Bash(ls:*)"], "deny": ["Read(~/.ssh/**)"], "defaultMode": "plan"},
  "statusLine": {"type": "command", "command": "mine"},
  "hooks": {
    "Stop": [{"hooks": [
      {"type": "command", "command": "orca-hook && echo '<done>'", "timeout": 10},
      {"type": "command", "command": "$HOME/.claude/bin/claude-code hook retired-hook", "timeout": 5}
    ]}],
    "SessionEnd": [{"hooks": [{"type": "command", "command": "foreign-end"}]}]
  },
  "enabledPlugins": {"x@y": true}
}`

func TestMergePreservesForeign(t *testing.T) {
	rendered, _ := Render()
	merged, sum, err := Merge([]byte(existing), rendered)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Env         map[string]string           `json:"env"`
		Theme       string                      `json:"theme"`
		Permissions map[string]json.RawMessage  `json:"permissions"`
		StatusLine  map[string]any              `json:"statusLine"`
		Hooks       map[string][]hookspec.Group `json:"hooks"`
		Plugins     map[string]bool             `json:"enabledPlugins"`
	}
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatalf("%v\n%s", err, merged)
	}
	if doc.Env["KEEP_ME"] != "1" || doc.Env["CLAUDE_CODE_API_KEY_HELPER_TTL_MS"] == "" {
		t.Errorf("env not unioned: %v", doc.Env)
	}
	if doc.Theme != "dark" || !doc.Plugins["x@y"] {
		t.Error("foreign top-level keys dropped")
	}
	if string(doc.Permissions["defaultMode"]) != `"plan"` || !strings.Contains(string(doc.Permissions["allow"]), "Bash(ls:*)") {
		t.Errorf("permissions not merged: %s", merged)
	}
	if strings.Count(string(doc.Permissions["deny"]), "Read(~/.ssh/**)") != 1 {
		t.Error("deny list duplicated an existing rule")
	}
	cmds := map[string][]string{}
	for ev, groups := range doc.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				cmds[ev] = append(cmds[ev], h.Command)
			}
		}
	}
	stop := strings.Join(cmds["Stop"], "\n")
	if !strings.Contains(stop, "orca-hook && echo '<done>'") || strings.Contains(stop, "retired-hook") || !strings.Contains(stop, "hook stop-format") {
		t.Errorf("Stop hooks wrong: %q", cmds["Stop"])
	}
	if len(cmds["SessionEnd"]) != 1 || len(cmds["PreToolUse"]) != 1 {
		t.Errorf("hooks: %v", cmds)
	}
	if strings.Contains(string(merged), "\\u0026") {
		t.Error("merge HTML-escaped a foreign command")
	}
	if sum.ForeignHooks != 2 || sum.HarnessHooks != len(hookspec.Registry) {
		t.Errorf("summary %+v", sum)
	}
	if !strings.HasPrefix(strings.Join(sum.Kept, ","), "theme,enabledPlugins") {
		t.Errorf("kept %v", sum.Kept)
	}
	again, _, err := Merge(merged, rendered)
	if err != nil || string(again) != string(merged) {
		t.Errorf("merge is not idempotent:\n%s", again)
	}
}

func TestMergeEmptyAndBackup(t *testing.T) {
	rendered, _ := Render()
	merged, _, err := Merge(nil, rendered)
	if err != nil || string(merged) != string(rendered) {
		t.Fatalf("merge onto nothing should equal the render: %v", err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "settings.json")
	if b, err := WriteWithBackup(file, []byte("{}\n")); err != nil || b != "" {
		t.Fatalf("fresh write: %q %v", b, err)
	}
	b, err := WriteWithBackup(file, merged)
	if err != nil || b == "" {
		t.Fatalf("backup: %q %v", b, err)
	}
	if old, _ := os.ReadFile(b); string(old) != "{}\n" {
		t.Errorf("backup content %q", old)
	}
}
