package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runAgentEffort runs the hook, returning the decoded updatedInput (nil when the hook emitted
// nothing) and the exit code.
func runAgentEffort(t *testing.T, stdin string) (map[string]any, int) {
	t.Helper()
	var out bytes.Buffer
	code := AgentEffort(strings.NewReader(stdin), &out)
	if strings.TrimSpace(out.String()) == "" {
		return nil, code
	}
	var doc struct {
		HSO struct {
			Event        string         `json:"hookEventName"`
			UpdatedInput map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("emitted invalid JSON (%v): %q", err, out.String())
	}
	if doc.HSO.Event != "PreToolUse" {
		t.Errorf("hookEventName = %q, want PreToolUse", doc.HSO.Event)
	}
	return doc.HSO.UpdatedInput, code
}

// The hook must never block — it is advisory plumbing, not a gate.
func TestAgentEffortAlwaysProceeds(t *testing.T) {
	for _, stdin := range []string{
		``,
		`not-json`,
		`{}`,
		`{"tool_name":"Agent"}`,
		`{"tool_name":"Agent","tool_input":{}}`,
		`{"tool_name":"Agent","tool_input":{"prompt":"fix a typo"}}`,
	} {
		if _, code := runAgentEffort(t, stdin); code != ExitProceed {
			t.Errorf("stdin %q exited %d, want ExitProceed", stdin, code)
		}
	}
}

func TestAgentEffortFillsUnsetModel(t *testing.T) {
	cases := []struct{ name, prompt, wantModel string }{
		{"low complexity -> haiku", "fix the typo in the help text", "haiku"},
		{"medium complexity -> sonnet", "add a --verbose flag to the token command", "sonnet"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := `{"tool_name":"Agent","tool_input":{"prompt":"` + c.prompt + `"}}`
			got, _ := runAgentEffort(t, in)
			if got == nil {
				t.Fatal("emitted nothing, want an updatedInput with a model")
			}
			if got["model"] != c.wantModel {
				t.Errorf("model = %v, want %q", got["model"], c.wantModel)
			}
		})
	}
}

// High complexity is PINNED to sonnet, not left to inherit. Inheriting meant a verbose prompt silently
// landed on whatever the parent session ran, which is how long audits ended up on the reasoning tier
// with no rule naming them. Whoever genuinely wants a bigger model sets model: explicitly — that wins.
func TestAgentEffortCapsHighAtSonnet(t *testing.T) {
	in := `{"tool_name":"Agent","tool_input":{"prompt":"refactor the auth ladder and migrate the hooks"}}`
	got, _ := runAgentEffort(t, in)
	if got == nil {
		t.Fatal("high-complexity dispatch was left to inherit the session model, want pinned model=sonnet")
	}
	if got["model"] != "sonnet" {
		t.Errorf("model = %v, want \"sonnet\"", got["model"])
	}
}

// An explicit model is a deliberate choice by whoever dispatched; the hook must not second-guess it
// even when the prompt classifies cheap.
func TestAgentEffortRespectsExplicitModel(t *testing.T) {
	in := `{"tool_name":"Agent","tool_input":{"prompt":"fix a typo","model":"opus"}}`
	if got, _ := runAgentEffort(t, in); got != nil {
		t.Errorf("explicit model was overridden to %v, want untouched", got)
	}
}

// The dispatch's own `model:"inherit"` is a Claude Code default, not a deliberate pin — it must be
// treated as unset, or a spawn left at "inherit" silently ran on the parent session's model instead
// of the classifier's cost-down tier.
func TestAgentEffortTreatsInheritModelAsUnset(t *testing.T) {
	in := `{"tool_name":"Agent","tool_input":{"prompt":"fix the typo in the help text","model":"inherit"}}`
	got, _ := runAgentEffort(t, in)
	if got == nil {
		t.Fatal("model:\"inherit\" was left untouched, want it filled like an unset model")
	}
	if got["model"] != "haiku" {
		t.Errorf("model = %v, want \"haiku\"", got["model"])
	}
}

// A roster agent that pins a concrete model already carries its tier — defer to the frontmatter.
// Hermetic: the roster is fabricated in a temp CLAUDE_CONFIG_DIR — reading the real repo roster
// would pass locally (ambient CLAUDE_CONFIG_DIR = the checkout) but fail in CI (empty ~/.claude).
func TestAgentEffortDefersToRosterPin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	writeAgent := func(name, model string) {
		body := "---\nname: " + name + "\ndescription: test agent\nmodel: " + model + "\n---\nbody\n"
		if err := os.MkdirAll(filepath.Join(home, "agents"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "agents", name+".md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeAgent("pinned-agent", "sonnet")
	writeAgent("inherit-agent", "inherit")

	pinned := `{"tool_name":"Agent","tool_input":{"prompt":"fix a typo","subagent_type":"pinned-agent"}}`
	if got, _ := runAgentEffort(t, pinned); got != nil {
		t.Errorf("model-pinned agent was overridden to %v, want untouched", got)
	}
	inherit := `{"tool_name":"Agent","tool_input":{"prompt":"fix a typo","subagent_type":"inherit-agent"}}`
	if got, _ := runAgentEffort(t, inherit); got == nil || got["model"] != "haiku" {
		t.Errorf("inherit-model agent = %v, want model=haiku", got)
	}
}

// updatedInput REPLACES the whole input record, so dropping a key would silently alter the dispatch.
// This is the change's sharpest edge — assert every original key survives byte-for-byte.
func TestAgentEffortPreservesAllInputKeys(t *testing.T) {
	in := `{"tool_name":"Agent","tool_input":{` +
		`"prompt":"fix the typo in the help text",` +
		`"description":"fix typo",` +
		`"isolation":"worktree",` +
		`"run_in_background":false,` +
		`"subagent_type":"general-purpose"}}`
	got, _ := runAgentEffort(t, in)
	if got == nil {
		t.Fatal("emitted nothing, want an updatedInput")
	}
	want := map[string]any{
		"prompt":            "fix the typo in the help text",
		"description":       "fix typo",
		"isolation":         "worktree",
		"run_in_background": false,
		"subagent_type":     "general-purpose",
		"model":             "haiku", // the only added key
	}
	if len(got) != len(want) {
		t.Fatalf("updatedInput has %d keys, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("updatedInput[%q] = %v, want %v", k, got[k], v)
		}
	}
}

// A non-spawn tool must be ignored even if the matcher is ever widened by mistake.
func TestAgentEffortIgnoresOtherTools(t *testing.T) {
	in := `{"tool_name":"Bash","tool_input":{"prompt":"fix a typo","command":"ls"}}`
	if got, _ := runAgentEffort(t, in); got != nil {
		t.Errorf("non-spawn tool was rewritten to %v, want untouched", got)
	}
}

// The matcher is "Agent|Task" (hookspec.Registry), and spawnTools (inflight.go) names both — Task
// must be filled exactly like Agent, not treated as inert.
func TestAgentEffortFillsUnsetModelOnTaskTool(t *testing.T) {
	in := `{"tool_name":"Task","tool_input":{"prompt":"fix a typo"}}`
	got, _ := runAgentEffort(t, in)
	if got == nil || got["model"] != "haiku" {
		t.Errorf("Task-tool dispatch = %v, want model=haiku", got)
	}
}

// With no subagent_type at all (a fresh general-purpose agent), the classifier still applies.
func TestAgentEffortNoSubagentType(t *testing.T) {
	in := `{"tool_name":"Agent","tool_input":{"prompt":"fix the typo in README","subagent_type":""}}`
	got, _ := runAgentEffort(t, in)
	if got == nil || got["model"] != "haiku" {
		t.Errorf("model = %v, want haiku", got)
	}
}

// A path-shaped subagent_type is not a roster name; the dispatch is left untouched rather than guessed at.
func TestAgentEffortRefusesPathShapedSubagentType(t *testing.T) {
	in := `{"tool_name":"Agent","tool_input":{"prompt":"fix a typo","subagent_type":"../etc/agents/x"}}`
	if got, _ := runAgentEffort(t, in); got != nil {
		t.Errorf("path-shaped subagent_type was rewritten to %v, want untouched", got)
	}
}
