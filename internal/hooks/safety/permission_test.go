package safety

import (
	"fmt"
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
		{"an allowed first segment never admits a denied second", `{"allow":[{"Tool":"Bash","Arg":"git log:*"}],"deny":[{"Tool":"Bash","Arg":"rm:*"}]}`,
			`{"tool_name":"Bash","tool_input":{"command":"git log && rm -rf x"}}`, true, "deny Bash(git log && rm -rf x)"},
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
		var body strings.Builder
		body.WriteString("*** Begin Patch\\n")
		for _, f := range files {
			body.WriteString("*** Add File: " + f + "\\n+x\\n")
		}
		return `{"tool_name":"Edit","cwd":"/repo","tool_input":{"patchText":"` + body.String() + `*** End Patch"}}`
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

// TestPermissionGateCodexApplyPatch: codex's apply_patch is judged as an Edit on each file its patch headers name.
func TestPermissionGateCodexApplyPatch(t *testing.T) {
	const patch = "*** Begin Patch\\n*** Update File: %s\\n@@\\n+x\\n*** End Patch"
	body := func(key string, files ...string) string {
		var text strings.Builder
		for _, f := range files {
			fmt.Fprintf(&text, patch, f)
		}
		return `{"tool_name":"apply_patch","cwd":"/repo","tool_input":{"` + key + `":"` + text.String() + `"}}`
	}
	cases := []struct {
		name, policy, payload string
		block                 bool
		reason                string
	}{
		{"denied file in command", `{"deny":[{"Tool":"Edit","Arg":"secret.txt"}]}`, body("command", "secret.txt"), true, "Edit(secret.txt)"},
		{"denied file in input", `{"deny":[{"Tool":"Edit","Arg":"secret.txt"}]}`, body("input", "secret.txt"), true, "Edit(secret.txt)"},
		{"denied file in patch", `{"deny":[{"Tool":"Edit","Arg":"secret.txt"}]}`, body("patch", "secret.txt"), true, "Edit(secret.txt)"},
		{"denied absolute path", `{"deny":[{"Tool":"Edit","Arg":"secret.txt"}]}`, body("command", "/repo/secret.txt"), true, "Edit(secret.txt)"},
		{"denied file second of two", `{"deny":[{"Tool":"Edit","Arg":"secret.txt"}]}`, body("command", "ok.txt", "secret.txt"), true, "Edit(secret.txt)"},
		{"plan mode refuses any patch", `{"mode":"plan"}`, body("command", "ok.txt"), true, "mode: plan"},
		{"headerless patch under a scoped deny", `{"deny":[{"Tool":"Edit","Arg":"secret.txt"}]}`,
			`{"tool_name":"apply_patch","cwd":"/repo","tool_input":{"command":"no headers"}}`, true, "has no path"},
		{"allowed file passes", `{"deny":[{"Tool":"Edit","Arg":"secret.txt"}]}`, body("command", "a.txt", "b.txt"), false, ""},
		{"no policy passes", ``, body("command", "secret.txt"), false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(agent.DispatchPermissionsEnv, c.policy)
			res := PermissionGate(core.ParseInputFor(strings.NewReader(c.payload), "codex"))
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

// TestPermissionGateUnknownTargetFailsClosed: a path tool with no discoverable target is refused when a scoped Deny covers it.
func TestPermissionGateUnknownTargetFailsClosed(t *testing.T) {
	t.Setenv(agent.DispatchPermissionsEnv, `{"deny":[{"Tool":"Edit","Arg":"secret.txt"},{"Tool":"Grep","Arg":"vault/**"}]}`)
	cases := []struct {
		name, payload string
		block         bool
		reason        string
	}{
		{"patch text with no header", `{"tool_name":"Edit","cwd":"/repo","tool_input":{"patchText":"  *** add file: secret.txt"}}`, true, "Edit("},
		{"grep without a path defaults to cwd and passes", `{"tool_name":"Grep","cwd":"/repo","tool_input":{"pattern":"k"}}`, false, ""},
		{"glob without a path has no Glob deny", `{"tool_name":"Glob","cwd":"/repo","tool_input":{"pattern":"*"}}`, false, ""},
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

func TestPermissionGateAbsoluteDenyPath(t *testing.T) {
	t.Setenv(agent.DispatchPermissionsEnv, `{"deny":[{"Tool":"Read","Arg":"/repo/secret.txt"}]}`)
	for _, path := range []string{"/repo/secret.txt", "secret.txt"} {
		payload := `{"tool_name":"Read","cwd":"/repo","tool_input":{"file_path":"` + path + `"}}`
		res := PermissionGate(core.ParseInput(strings.NewReader(payload)))
		if res.Decision != core.Block || !strings.Contains(res.Reason, "deny Read(/repo/secret.txt)") {
			t.Errorf("path %q: decision %v reason %q, want absolute-path deny", path, res.Decision, res.Reason)
		}
	}
	payload := `{"tool_name":"Read","cwd":"/repo","tool_input":{"file_path":"/repo/public.txt"}}`
	if res := PermissionGate(core.ParseInput(strings.NewReader(payload))); res.Decision == core.Block {
		t.Errorf("unrelated path blocked: %s", res.Reason)
	}
}

func TestPermissionGateTruncatedInputFailsClosed(t *testing.T) {
	const payload = `{"tool_name":"Read","cwd":"/repo","tool_input":{"file_path":"/repo/secret.txt"`
	for _, tc := range []struct {
		policy string
		want   core.Decision
	}{
		{`{"deny":[{"Tool":"Read","Arg":"/repo/secret.txt"}]}`, core.Block},
		{`{"mode":"plan"}`, core.Block},
		{"", core.Allow},
	} {
		t.Setenv(agent.DispatchPermissionsEnv, tc.policy)
		res := PermissionGate(core.ParseInput(strings.NewReader(payload)))
		if res.Decision != tc.want {
			t.Errorf("policy %q: decision %v reason %q, want %v", tc.policy, res.Decision, res.Reason, tc.want)
		}
	}
}

// TestPermissionGateRedirectDenialSaysHowToRetry: a memory job's `2>&1` is refused with the redirect-free retry, not "report it".
func TestPermissionGateRedirectDenialSaysHowToRetry(t *testing.T) {
	t.Setenv(agent.DispatchPermissionsEnv, `{"mode":"plan","allow":[{"Tool":"Bash","Arg":"claude-code memory review *"}],"deny":[{"Tool":"Bash","Arg":"*>*"}]}`)
	bash := func(cmd string) core.Result {
		return PermissionGate(core.ParseInput(strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"` + cmd + `"}}`)))
	}
	if res := bash("claude-code memory review"); res.Decision == core.Block {
		t.Fatalf("the bare command must pass, got %q", res.Reason)
	}
	res := bash("claude-code memory review 2>&1")
	if res.Decision != core.Block || !strings.Contains(res.Reason, "again without them") || strings.Contains(res.Reason, "Report it") {
		t.Errorf("decision %v reason %q, want a block telling the model to retry without redirects", res.Decision, res.Reason)
	}
	res = bash("wc -l x.md")
	if res.Decision != core.Block || !strings.Contains(res.Reason, "allow: Bash(claude-code memory review *)") || strings.Contains(res.Reason, "Report it") {
		t.Errorf("decision %v reason %q, want a block naming the allow list and telling the model to continue", res.Decision, res.Reason)
	}
}

// TestPermissionGatePlanAllowsAbsoluteGlobUnderCWD: a bank file below the cwd meets an absolute allow glob; outside it is refused.
func TestPermissionGatePlanAllowsAbsoluteGlobUnderCWD(t *testing.T) {
	t.Setenv(agent.DispatchPermissionsEnv, `{"mode":"plan","allow":[{"Tool":"Edit","Arg":"/repo/memory/**"},{"Tool":"Write","Arg":"/repo/memory/**"}],`+
		`"deny":[{"Tool":"Edit","Arg":"/repo/memory/.git/**"},{"Tool":"Write","Arg":"/repo/memory/.git/**"}]}`)
	edit := func(path string) core.Decision {
		return PermissionGate(core.ParseInput(strings.NewReader(
			`{"tool_name":"Edit","cwd":"/repo","tool_input":{"file_path":"` + path + `"}}`))).Decision
	}
	for path, block := range map[string]bool{
		"/repo/memory/bank/progress.md": false, "memory/bank/progress.md": false,
		"/repo/main.go": true, "/repo/memory/.git/config": true, "memory/.git/config": true,
	} {
		if got := edit(path) == core.Block; got != block {
			t.Errorf("Edit(%s) blocked = %v, want %v", path, got, block)
		}
	}
}
