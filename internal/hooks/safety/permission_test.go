package safety

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/settings"
)

// TestPermissionGate drives the gate with real hook JSON under each env shape the dispatch can hand a child.
func TestPermissionGate(t *testing.T) {
	const policy = `{"deny":[{"Tool":"Bash","Arg":"rm *"},{"Tool":"Edit","Arg":".env"}]}`
	cases := []struct {
		name, env, payload string
		block              bool
		reason             string
	}{
		{"unset env is a no-op", "", `{"tool_name":"Bash","tool_input":{"command":"rm x"}}`, false, ""},
		{"deny blocks rm", policy, `{"tool_name":"Bash","tool_input":{"command":"rm v1"}}`, true, "deny Bash(rm v1)"},
		{"deny blocks bash -c rm", policy, `{"tool_name":"Bash","tool_input":{"command":"bash -c \"rm v3\""}}`, true, `Bash(bash -c "rm v3")`},
		{"deny blocks a protected path in cwd", policy,
			`{"tool_name":"write","cwd":"/repo","tool_input":{"filePath":"/repo/.env","content":"k"}}`, true, "deny Edit(.env)"},
		{"allowed command passes", policy, `{"tool_name":"Bash","tool_input":{"command":"ls -la"}}`, false, ""},
		{"malformed env blocks", `{deny`, `{"tool_name":"Bash","tool_input":{"command":"ls"}}`, true, "is unreadable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(agent.DispatchPermissionsEnv, c.env)
			res := PermissionGate(core.ParseInput(strings.NewReader(c.payload)))
			if (res.Decision == core.Block) != c.block || !strings.Contains(res.Reason, c.reason) {
				t.Errorf("decision %v reason %q, want block=%v naming %q", res.Decision, res.Reason, c.block, c.reason)
			}
		})
	}
}

// TestPermissionGateApplyPatchJudgesEachFile: an opencode apply_patch is refused when any one of its files is denied.
func TestPermissionGateApplyPatchJudgesEachFile(t *testing.T) {
	t.Setenv(agent.DispatchPermissionsEnv, `{"deny":[{"Tool":"Edit","Arg":"secret.txt"},{"Tool":"Grep","Arg":"vault"}]}`)
	patch := func(files ...string) string {
		body := "*** Begin Patch\\n"
		for _, f := range files {
			body += "*** Add File: " + f + "\\n+x\\n"
		}
		return `{"tool_name":"Edit","cwd":"/repo","tool_input":{"patchText":"` + body + `*** End Patch"}}`
	}
	cases := []struct {
		name, payload string
		block         bool
		reason        string
	}{
		{"denied file alone", patch("/repo/secret.txt"), true, "Edit(secret.txt)"},
		{"denied file second of two", patch("ok.txt", "secret.txt"), true, "Edit(secret.txt)"},
		{"moved onto a denied path", `{"tool_name":"Edit","cwd":"/repo","tool_input":{"patchText":"*** Update File: a.txt\n*** Move to: secret.txt\n"}}`, true, "Edit(secret.txt)"},
		{"allowed files", patch("a.txt", "b.txt"), false, ""},
		{"grep on a denied dir", `{"tool_name":"Grep","cwd":"/repo","tool_input":{"path":"/repo/vault","pattern":"k"}}`, true, "Grep(vault)"},
		{"glob unlisted passes", `{"tool_name":"Glob","cwd":"/repo","tool_input":{"path":"src"}}`, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := PermissionGate(core.ParseInput(strings.NewReader(c.payload)))
			if (res.Decision == core.Block) != c.block || !strings.Contains(res.Reason, c.reason) {
				t.Errorf("decision %v reason %q, want block=%v naming %q", res.Decision, res.Reason, c.block, c.reason)
			}
		})
	}
}

// TestPermissionGateAgentPayloads: the captured codex and agy PreToolUse payloads map to the rule tool the Deny names.
func TestPermissionGateAgentPayloads(t *testing.T) {
	t.Setenv(agent.DispatchPermissionsEnv, `{"deny":[{"Tool":"Bash","Arg":"rm *"},{"Tool":"Read","Arg":"s.txt"},{"Tool":"Edit","Arg":"n.txt"}]}`)
	cases := []struct {
		name, agent, payload, reason string
	}{
		{"codex Bash compound", "codex", `{"session_id":"s","cwd":"/w","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"echo hi && rm v2"}}`, "Bash(echo hi && rm v2)"},
		{"agy run_command subshell", "gemini", `{"toolCall":{"name":"run_command","args":{"CommandLine":"(rm v4)"}},"workspacePaths":["/w"]}`, "Bash((rm v4))"},
		{"agy read_file", "gemini", `{"toolCall":{"name":"read_file","args":{"Path":"/w/s.txt"}},"workspacePaths":["/w"]}`, "Read(s.txt)"},
		{"agy write_file", "gemini", `{"toolCall":{"name":"write_file","args":{"Path":"/w/n.txt","Content":"a"}},"workspacePaths":["/w"]}`, "Edit(n.txt)"},
		{"agy edit_file", "gemini", `{"toolCall":{"name":"edit_file","args":{"Path":"/w/n.txt","Target":"a","Replacement":"b"}},"workspacePaths":["/w"]}`, "Edit(n.txt)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(settings.AgentEnv, c.agent)
			res := PermissionGate(core.ParseInput(strings.NewReader(c.payload)))
			if res.Decision != core.Block || !strings.Contains(res.Reason, c.reason) {
				t.Errorf("decision %v reason %q, want a block naming %s", res.Decision, res.Reason, c.reason)
			}
		})
	}
	t.Setenv(settings.AgentEnv, "gemini")
	if res := PermissionGate(core.ParseInput(strings.NewReader(`{"toolCall":{"name":"run_command","args":{"CommandLine":"touch ok.txt"}}}`))); res.Decision == core.Block {
		t.Errorf("an allowed agy command was blocked: %q", res.Reason)
	}
}
