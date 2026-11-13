package claude

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/safety"
)

const dbxProfiles = `{"providers":{"databricks":{"qa":{"preset":"databricks","host":"https://qa.cloud.databricks.com","auth":{"type":"pat"}}}}}`

// launchOf composes claudeExec's launch for req (gate decision, args, command) without starting the child.
func launchOf(t *testing.T, req agent.RunOptions) (agent.RunOptions, []string, []string) {
	t.Helper()
	ctx, req := gateOffAnthropic(context.Background(), req)
	req.Launcher = claudeLauncher{}
	cmd, _, _, err := claudeCmd(ctx, req, claudeArgs([]string{"--print"}, req), "")
	if err != nil {
		t.Fatalf("claudeCmd: %v", err)
	}
	return req, cmd.Args, cmd.Env
}

// envPolicy is the Permissions the child env hands the gate.
func envPolicy(t *testing.T, env []string) (p agent.Permissions) {
	t.Helper()
	i := slices.IndexFunc(env, func(e string) bool { return strings.HasPrefix(e, agent.DispatchPermissionsEnv+"=") })
	if i < 0 {
		t.Fatalf("child env carries no %s", agent.DispatchPermissionsEnv)
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(env[i], agent.DispatchPermissionsEnv+"=")), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// An off-Anthropic --tools launch is the gated form; the Anthropic route keeps its judge and its flags.
func TestGateOffAnthropicLaunchArgs(t *testing.T) {
	hermetic(t, dbxProfiles)
	wd := t.TempDir()
	req, args, env := launchOf(t, agent.RunOptions{Tools: true, Provider: "qa", WorkDir: wd,
		Permissions: agent.Permissions{Allow: []agent.Rule{{Tool: "Bash", Arg: "curl --version"}}}})
	joined := strings.Join(args, " ")
	for _, want := range []string{"--permission-mode bypassPermissions", "--strict-mcp-config", "--tools " + gateTools,
		"--permission-gate || exit 2"} {
		if !strings.Contains(joined, want) {
			t.Errorf("gated args %q lack %q", joined, want)
		}
	}
	for _, not := range []string{"dontAsk", "--allowedTools", "--permission-prompts"} {
		if strings.Contains(joined, not) {
			t.Errorf("gated args %q must not carry %q", joined, not)
		}
	}
	got := envPolicy(t, env)
	if !req.Gate || got.Mode != "plan" || !slices.Contains(got.Allow, agent.Rule{Tool: "Bash", Arg: "curl --version"}) {
		t.Errorf("gate %v, env policy %+v: want plan mode with the user's allow", req.Gate, got)
	}
}

// The Anthropic route keeps its judge: no gate, no bypassPermissions.
func TestGateOffAnthropicSkipsAnthropic(t *testing.T) {
	hermetic(t, dbxProfiles)
	anth, aargs, _ := launchOf(t, agent.RunOptions{Tools: true, Provider: "ultra", WorkDir: t.TempDir()})
	if anth.Gate || strings.Contains(strings.Join(aargs, " "), "bypassPermissions") {
		t.Errorf("anthropic route must stay ungated and judged, got gate=%v args %q", anth.Gate, aargs)
	}
}

// A bare --autonomous off Anthropic is the gated launch, never bare bypassPermissions; --safe-mode is refused.
func TestGateOffAnthropicGatesBareAutonomous(t *testing.T) {
	hermetic(t, dbxProfiles)
	req, args, env := launchOf(t, agent.RunOptions{Tools: true, Autonomous: true, Provider: "qa", WorkDir: t.TempDir()})
	if joined := strings.Join(args, " "); !req.Gate || !strings.Contains(joined, "--permission-gate || exit 2") ||
		!strings.Contains(joined, "--tools "+gateTools) || envPolicy(t, env).Mode != "plan" {
		t.Errorf("bare autonomous off Anthropic: gate=%v args %q, want the gated launch", req.Gate, args)
	}
	bare := []string{"--permission-mode", "bypassPermissions"}
	if err := refuseUngatedBypass(bare, agent.RunOptions{Tools: true, Autonomous: true, Provider: "qa", SafeMode: true}); err == nil {
		t.Error("ungated bypassPermissions off Anthropic must be refused")
	}
	if err := refuseUngatedBypass(bare, agent.RunOptions{Tools: true, Autonomous: true, Provider: "ultra"}); err != nil {
		t.Errorf("bare autonomous on Anthropic keeps its launch, got %v", err)
	}
}

// Plan mode, --safe-mode and no tools keep their launches; bypassPermissions never goes ungated.
func TestGateOffAnthropicKeepsOtherLaunches(t *testing.T) {
	hermetic(t, dbxProfiles)
	for name, req := range map[string]agent.RunOptions{
		"plan mode": {Tools: true, Provider: "qa", Permissions: agent.Permissions{Mode: "plan"}},
		"safe mode": {Tools: true, Provider: "qa", SafeMode: true},
		"no tools":  {Provider: "qa"},
	} {
		if got, _, _ := launchOf(t, req); got.Gate {
			t.Errorf("%s: must not take the gated launch", name)
		}
	}
	if err := refuseUngatedBypass([]string{"--permission-mode", "bypassPermissions"}, agent.RunOptions{Tools: true}); err == nil {
		t.Error("bypassPermissions without a gate (or the bare --autonomous) must be refused")
	}
}

// gateCall runs the real permission-gate on one tool call under policy, as the child's hook would.
func gateCall(t *testing.T, policy agent.Permissions, cwd, tool string, input map[string]any) core.Result {
	t.Helper()
	raw, _ := json.Marshal(policy)
	t.Setenv(agent.DispatchPermissionsEnv, string(raw))
	payload, _ := json.Marshal(map[string]any{"tool_name": tool, "cwd": cwd, "tool_input": input})
	return safety.PermissionGate(core.ParseInput(strings.NewReader(string(payload))))
}

// The built policy through the gate's Decide path: the workdir is writable, everything unlisted is refused, deny wins.
func TestGatePolicyDecisions(t *testing.T) {
	wd := t.TempDir()
	rule := func(s string) agent.Rule { r, _ := agent.ParseRule(s); return r }
	write := func(path string) map[string]any { return map[string]any{"file_path": path, "content": "ok"} }
	bash := func(cmd string) map[string]any { return map[string]any{"command": cmd} }
	const miss = "continue with an allowed call"
	cases := []struct {
		name        string
		user        agent.Permissions
		tool        string
		input       map[string]any
		block       bool
		reason, not string
	}{
		{"write inside the workdir", agent.Permissions{}, "Write", write(filepath.Join(wd, "a.txt")), false, "", ""},
		{"write outside the workdir", agent.Permissions{}, "Write", write("/tmp/outside-probe.txt"), true,
			"--allow 'Edit(/tmp/outside-probe.txt)'", ""},
		{"write in a sibling of the workdir", agent.Permissions{}, "Write", write(filepath.Join(filepath.Dir(wd), "sib.txt")),
			true, "deny: ", ""},
		{"the hinted --allow Edit opens a Write", agent.Permissions{Allow: []agent.Rule{rule("Edit(/tmp/open/**)")}},
			"Write", write("/tmp/open/x.txt"), false, "", ""},
		{"user --allow Write alone opens a path", agent.Permissions{Allow: []agent.Rule{rule("Write(/tmp/open/**)")}},
			"Write", write("/tmp/open/x.txt"), false, "", ""},
		{"user --deny beats the workdir allow", agent.Permissions{Deny: []agent.Rule{rule("Write(" + wd + "/state/**)")}},
			"Write", write(filepath.Join(wd, "state", "p.txt")), true, "deny: Write(", "re-dispatch"},
		{"user --ask has no judge, so it refuses", agent.Permissions{Ask: []agent.Rule{rule("Edit(" + wd + "/ask/**)")}},
			"Edit", write(filepath.Join(wd, "ask", "p.txt")), true, "deny: Edit(", ""},
		{"unlisted bash", agent.Permissions{}, "Bash", bash("curl --version"), true, miss + "; do not retry", ""},
		{"unlisted bash names the rule to add", agent.Permissions{}, "Bash", bash("curl --version"), true,
			"--allow 'Bash(curl --version)'", ""},
		{"listed bash", agent.Permissions{}, "Bash", bash("go vet ./..."), false, "", ""},
		{"user-allowed bash", agent.Permissions{Allow: []agent.Rule{rule("Bash(curl --version)")}}, "Bash",
			bash("curl --version"), false, "", ""},
		{"redirect out of a listed command", agent.Permissions{}, "Bash", bash("ls > /tmp/x"), true, "without them", ""},
		{"user --deny beats a listed command", agent.Permissions{Deny: []agent.Rule{rule("Bash(go vet *)")}}, "Bash",
			bash("go vet ./..."), true, "deny: Bash(go vet *)", "re-dispatch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := gateCall(t, gatePolicy(c.user, wd), wd, c.tool, c.input)
			if (res.Decision == core.Block) != c.block || !strings.Contains(res.Reason, c.reason) ||
				c.not != "" && strings.Contains(res.Reason, c.not) {
				t.Fatalf("decision %v reason %q, want block=%v naming %q and not %q", res.Decision, res.Reason, c.block,
					c.reason, c.not)
			}
		})
	}
}

// A gated launch with no policy, no gate binary, no policy env or --safe-mode is refused, never run ungated.
func TestGatedLaunchRefusedWithoutItsParts(t *testing.T) {
	hermetic(t, "")
	policy := agent.Permissions{Mode: "plan", Allow: []agent.Rule{{Tool: "Read"}}}
	oldExe := gateExe
	t.Cleanup(func() { gateExe = oldExe })
	cases := []struct {
		name string
		req  agent.RunOptions
		ctx  context.Context
		exe  func() (string, error)
		want string
	}{
		{"no policy", agent.RunOptions{}, agent.WithPermissionsEnv(context.Background(), agent.Permissions{}), oldExe, "no permission policy"},
		{"no gate binary", agent.RunOptions{Permissions: policy}, agent.WithPermissionsEnv(context.Background(), policy),
			func() (string, error) { return "", errors.New("gone") }, "permission-gate binary"},
		{"binary path missing", agent.RunOptions{Permissions: policy}, agent.WithPermissionsEnv(context.Background(), policy),
			func() (string, error) { return filepath.Join(t.TempDir(), "absent"), nil }, "permission-gate binary"},
		{"no policy env", agent.RunOptions{Permissions: policy}, context.Background(), oldExe, "carries no " + agent.DispatchPermissionsEnv},
		{"safe mode", agent.RunOptions{Permissions: policy, SafeMode: true}, agent.WithPermissionsEnv(context.Background(), policy),
			oldExe, "--safe-mode"},
	}
	for _, c := range cases {
		gateExe = c.exe
		req := c.req
		req.Tools, req.Gate, req.Launcher = true, true, claudeLauncher{}
		_, _, _, err := claudeCmd(c.ctx, req, claudeArgs([]string{"--print"}, req), "")
		if err == nil || !strings.Contains(err.Error(), "gated launch refused") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want a gated-launch refusal naming %q", c.name, err, c.want)
		}
	}
}
