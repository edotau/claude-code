package safety

import (
	"bytes"
	"encoding/json"
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
		{"a read-only miss names the segment and the rewrite", `{"deny":[{"Tool":"Bash","Arg":"not a read-only shell command"}]}`,
			`{"tool_name":"Bash","tool_input":{"command":"git status && rm /tmp/f"}}`, true, "denied segment: rm /tmp/f\n"},
		{"a relative plan allow meets the absolute path", `{"mode":"plan","allow":[{"Tool":"Edit","Arg":"sub/**"},{"Tool":"Write","Arg":"sub/**"}]}`,
			`{"tool_name":"Edit","cwd":"/repo","tool_input":{"file_path":"sub/a.txt"}}`, false, ""},
		{"a relative cd is judged from the hook cwd", `{"mode":"plan","allow":[{"Tool":"Bash","Arg":"cd:/repo"}]}`,
			`{"tool_name":"Bash","cwd":"/repo","tool_input":{"command":"cd sub && cd deeper"}}`, false, ""},
		{"a relative cd out of the allowed dir", `{"mode":"plan","allow":[{"Tool":"Bash","Arg":"cd:/repo"}]}`,
			`{"tool_name":"Bash","cwd":"/repo","tool_input":{"command":"cd .."}}`, true, "Bash(cd ..) is not on it: the nearest"},
		{"a relative cd into a denied dir", `{"allow":[{"Tool":"Bash","Arg":"cd *"}],"deny":[{"Tool":"Bash","Arg":"cd:/repo/secret"}]}`,
			`{"tool_name":"Bash","cwd":"/repo","tool_input":{"command":"cd sub/../secret"}}`, true, "deny Bash(cd sub/../secret)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(agent.DispatchPermissionsEnv, c.env)
			res := PermissionGate(core.ParseInput(strings.NewReader(c.payload)))
			if (res.Decision == core.Block) != c.block || res.Floor != c.block || !strings.Contains(res.Reason, c.reason) {
				t.Errorf("decision %v reason %q, want block=%v naming %q", res.Decision, res.Reason, c.block, c.reason)
			}
		})
	}
}

// TestPermissionGateApplyPatchJudgesEachFile: an opencode apply_patch is refused when any one of its files is denied.
// TestPermissionGatePi: pi's edit calls follow the dispatch rules like opencode's.
func TestPermissionGatePi(t *testing.T) {
	t.Setenv(agent.DispatchPermissionsEnv, `{"deny":[{"Tool":"Edit","Arg":"notes.txt"}]}`)
	for _, c := range []struct {
		path  string
		block bool
	}{{"/r/notes.txt", true}, {"/r/other.txt", false}} {
		payload := `{"tool_name":"Edit","harness_agent":"pi","cwd":"/r","tool_input":{"file_path":"` + c.path + `"}}`
		res := PermissionGate(core.ParseInput(strings.NewReader(payload)))
		if (res.Decision == core.Block) != c.block {
			t.Errorf("%s: decision %v, want block=%v (%s)", c.path, res.Decision, c.block, res.Reason)
		}
	}
}

func TestPermissionGateApplyPatchJudgesEachFile(t *testing.T) {
	t.Setenv(agent.DispatchPermissionsEnv, `{"deny":[{"Tool":"Edit","Arg":"secret.txt"},{"Tool":"Grep","Arg":"vault"}]}`)
	patch := func(files ...string) string {
		var body strings.Builder
		body.WriteString("*** Begin Patch\\n")
		for _, f := range files {
			body.WriteString("*** Add File: ")
			body.WriteString(f)
			body.WriteString("\\n+x\\n")
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
		{"no rules: protected ssh dir", ``, body("command", "~/.ssh/authorized_keys2"), true, "authorized_keys2"},
		{"no rules: temp file", ``, body("command", "/tmp/fixture/a.txt"), false, ""},
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

func TestPermissionGateReadOnlyCD(t *testing.T) {
	p := agent.Permissions{Mode: "plan", Allow: []agent.Rule{agent.ReadOnlyAllow, agent.CdRule("/memory")},
		Deny: append([]agent.Rule{{Tool: "Read", Arg: "**/.env"}, {Tool: "Bash", Arg: "cd:/private"}}, agent.ReadOnlyDeny...)}
	setGatePermissions(t, p)
	for cmd, block := range map[string]bool{
		"cd /memory && wc -l progress.md && head -30 sessionHistory.md":     false,
		"cd /memory && rtk read sessionHistory.md | head -40":               false,
		"cd /memory && rtk wc -l progress.md":                               false,
		"cd /memory && rtk head -30 progress.md && rtk tail -5 progress.md": false,
		"cd /memory && cat .env":                                            true,
		"cd /memory && rtk read .env":                                       true,
		"cd /memory && rtk head -f progress.md":                             true,
		"cd /memory && cat progress.md > out.md":                            true,
		"cd /memory && rm progress.md":                                      true,
		"cd /memory && sed -i s/a/b/ progress.md":                           true,
		"cd /memory && cd /private && cat f":                                true,
		"cd /memory-evil && cat f":                                          true,
	} {
		payload := fmt.Sprintf(`{"tool_name":"Bash","cwd":"/","tool_input":{"command":%q}}`, cmd)
		r := PermissionGate(core.ParseInput(strings.NewReader(payload)))
		if (r.Decision == core.Block) != block {
			t.Errorf("%s: decision %v reason %q, want block=%v", cmd, r.Decision, r.Reason, block)
		}
	}
}

func TestPermissionGateMissingToolReason(t *testing.T) {
	t.Setenv(agent.DispatchPermissionsEnv, `{"mode":"plan"}`)
	for payload, want := range map[string]string{
		"": "empty hook payload", "{": "unexpected end of JSON input",
		"{}": "missing tool_name in hook payload", "null": "payload is not a JSON object",
	} {
		r := PermissionGate(core.ParseInput(strings.NewReader(payload)))
		if r.Decision != core.Block || !strings.Contains(r.Reason, want) {
			t.Errorf("payload %q: decision %v reason %q, want %q", payload, r.Decision, r.Reason, want)
		}
	}
}

// TestPermissionGateRedirectDenialSaysHowToRetry: a memory job's `2>&1` is refused with the redirect-free retry, not "report it".
func TestPermissionGateRedirectDenialSaysHowToRetry(t *testing.T) {
	t.Setenv(agent.DispatchPermissionsEnv, `{"mode":"plan","allow":[{"Tool":"Bash","Arg":"claude-code memory review *"}],"deny":[{"Tool":"Bash","Arg":"`+agent.RedirectFileArg+`"}]}`)
	bash := func(cmd string) core.Result {
		return PermissionGate(core.ParseInput(strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"` + cmd + `"}}`)))
	}
	if res := bash("claude-code memory review"); res.Decision == core.Block {
		t.Fatalf("the bare command must pass, got %q", res.Reason)
	}
	if res := bash("claude-code memory review 2>&1"); res.Decision == core.Block {
		t.Fatalf("descriptor duplication must pass, got %q", res.Reason)
	}
	res := bash("claude-code memory review > out.md")
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

type gateRecoveryCase struct{ command, deny, tool, arg string }

func TestPermissionGateContractRecovery(t *testing.T) {
	for _, tc := range []gateRecoveryCase{
		{command: "wc -l report.txt", tool: "Bash", arg: "wc -l report.txt"},
		{command: "wc -l *.txt", tool: "Bash", arg: "wc -l *.txt"},
		{command: "wc -l report.txt", deny: "Bash(wc *)", tool: "Bash", arg: "wc -l report.txt"},
		{deny: "Write(secret.txt)", tool: "Edit", arg: "secret.txt"},
	} {
		checkGateRecovery(t, tc)
	}
}

func mustParseRule(t *testing.T, spec string) agent.Rule {
	t.Helper()
	r, err := agent.ParseRule(spec)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// setGatePermissions sets p as the dispatch's permission env the gate reads.
func setGatePermissions(t *testing.T, p agent.Permissions) {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(agent.DispatchPermissionsEnv, string(data))
}

// checkGateRecovery expects the gate to deny tc with the shared remedy (or the deciding deny rule) and each vendor to relay it.
func checkGateRecovery(t *testing.T, tc gateRecoveryCase) {
	t.Helper()
	p := agent.Permissions{Mode: "plan", Allow: []agent.Rule{{Tool: "Bash", Arg: "ls"}}}
	if tc.deny != "" {
		p.Deny = []agent.Rule{mustParseRule(t, tc.deny)}
	}
	payload := fmt.Sprintf(`{"tool_name":%q,"tool_input":{"command":%q,"file_path":%q}}`, tc.tool, tc.command, tc.arg)
	setGatePermissions(t, p)
	r := PermissionGate(core.ParseInput(strings.NewReader(payload)))
	if r.Decision != core.Block {
		t.Fatalf("expected denial: %+v", tc)
	}
	d := p.Explain(tc.tool, tc.arg)
	wantAllow := tc.deny == "" && d.Replay != ""
	if strings.Contains(r.Reason, "--allow") != wantAllow {
		t.Fatalf("invalid remedy: %s", r.Reason)
	}
	if wantAllow {
		if !strings.Contains(r.Reason, d.Remedy) {
			t.Fatalf("missing shared remedy: %s", r.Reason)
		}
		p.Allow = append(p.Allow, agent.Rule{Tool: tc.tool, Arg: tc.arg})
		setGatePermissions(t, p)
		if replay := PermissionGate(core.ParseInput(strings.NewReader(payload))); replay.Decision == core.Block {
			t.Fatalf("replay denied: %s", replay.Reason)
		}
	}
	if tc.deny != "" && !strings.Contains(r.Reason, "source: permissions.deny rule: "+tc.deny) {
		t.Fatalf("missing deciding rule: %s", r.Reason)
	}
	checkVendorsRelayReason(t, r)
}

// checkVendorsRelayReason expects every vendor's PreToolUse emission to carry r's reason.
func checkVendorsRelayReason(t *testing.T, r core.Result) {
	t.Helper()
	for _, vendor := range []string{core.AgentClaude, core.AgentCodex, core.AgentCopilot, core.AgentGemini} {
		var out, stderr bytes.Buffer
		core.Emit(vendor, "PreToolUse", r, &out, &stderr)
		var doc any
		text := stderr.String()
		if out.Len() > 0 {
			if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			text += fmt.Sprint(doc)
		}
		if !strings.Contains(text, r.Reason) {
			t.Fatalf("%s lost reason: %s", vendor, text)
		}
	}
}

// A leaf's apply_patch on settings.json is refused with no dispatch policy; the main session's is not.
func TestPermissionGateLeafSettingsPatch(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HARNESS_DISPATCH_ID", "")
	t.Setenv("HARNESS_DISPATCH_DEPTH", "")
	t.Setenv(agent.DispatchPermissionsEnv, "")
	for _, c := range []struct {
		agentID string
		block   bool
	}{{"a1", true}, {"", false}} {
		payload, _ := json.Marshal(map[string]any{"tool_name": "apply_patch", "cwd": "/tmp", "agent_id": c.agentID,
			"tool_input": map[string]string{"command": "*** Begin Patch\n*** Update File: " + cfg + "/settings.json\n@@\n+x\n*** End Patch"}})
		res := PermissionGate(core.ParseInputFor(bytes.NewReader(payload), "codex"))
		if (res.Decision == core.Block) != c.block {
			t.Errorf("agent_id %q: decision %v reason %q, want block=%v", c.agentID, res.Decision, res.Reason, c.block)
		}
	}
}

// dispatchAsk: the dispatch's own --allow/--deny rules decide a settings ask; with neither, the block names the flag.
func TestDispatchAsk(t *testing.T) {
	hit := askHit{what: "settings ask", tool: "Bash", arg: "go build ./...", rule: "Bash(go build *)"}
	rules := func(specs ...string) []agent.Rule {
		var out []agent.Rule
		for _, s := range specs {
			out = append(out, mustParseRule(t, s))
		}
		return out
	}
	for _, c := range []struct {
		name   string
		policy *agent.Permissions
		want   core.Decision
		reason string
	}{
		{"allow", &agent.Permissions{Allow: rules("Bash(go build:*)")}, core.Allow, ""},
		{"deny", &agent.Permissions{Deny: rules("Bash(go build:*)"), Allow: rules("Bash")}, core.Block, "--deny"},
		{"neither with the hint", &agent.Permissions{Allow: rules("Bash(ls:*)")}, core.Block, "--allow 'Bash(go build *)'"},
		{"no policy with the hint", nil, core.Block, "--allow 'Bash(go build *)'"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(agent.DispatchPermissionsEnv, "")
			if c.policy != nil {
				setGatePermissions(t, *c.policy)
			}
			got, reason := dispatchAsk(&core.Input{CWD: "/tmp"}, hit)
			if got != c.want || !strings.Contains(reason, c.reason) {
				t.Errorf("dispatchAsk = %v %q, want %v containing %q", got, reason, c.want, c.reason)
			}
		})
	}
}
