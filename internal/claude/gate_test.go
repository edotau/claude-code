package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/safety"
	"github.com/edotau/claude-code/internal/paths"
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
	if !req.Gate || got.Mode != "" || !slices.Contains(got.Allow, agent.Rule{Tool: "Bash", Arg: "curl --version"}) {
		t.Errorf("gate %v, env policy %+v: want only the user's allow", req.Gate, got)
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

// A bare --autonomous off Anthropic runs bare under bypassPermissions; --safe-mode is refused.
func TestGateOffAnthropicBareAutonomous(t *testing.T) {
	hermetic(t, dbxProfiles)
	req, args, _ := launchOf(t, agent.RunOptions{Tools: true, Autonomous: true, Provider: "qa", WorkDir: t.TempDir()})
	if joined := strings.Join(args, " "); req.Gate || !strings.Contains(joined, "bypassPermissions") ||
		strings.Contains(joined, "--permission-gate") || strings.Contains(joined, "--tools "+gateTools) {
		t.Errorf("bare autonomous off Anthropic: gate=%v args %q, want bare bypassPermissions", req.Gate, args)
	}
	bare := []string{"--permission-mode", "bypassPermissions"}
	if err := refuseUngatedBypass(context.Background(), bare, agent.RunOptions{Tools: true, Autonomous: true, Provider: "qa", SafeMode: true}); err == nil {
		t.Error("ungated bypassPermissions off Anthropic with --safe-mode must be refused")
	}
	if err := refuseUngatedBypass(context.Background(), bare, agent.RunOptions{Tools: true, Autonomous: true, Provider: "ultra"}); err != nil {
		t.Errorf("bare autonomous on Anthropic keeps its launch, got %v", err)
	}
}

// A default off-Anthropic --tools launch has no implicit policy; --read-only and explicit rules still gate.
func TestGateOffAnthropicImplicitPolicy(t *testing.T) {
	hermetic(t, dbxProfiles)
	deny := agent.Rule{Tool: "Bash", Arg: "curl *"}
	ask := agent.Rule{Tool: "Bash", Arg: "rm *"}
	def, dargs, _ := launchOf(t, agent.RunOptions{Tools: true, Provider: "qa", WorkDir: t.TempDir()})
	if joined := strings.Join(dargs, " "); def.Gate || !strings.Contains(joined, "bypassPermissions") ||
		strings.Contains(joined, "dontAsk") || strings.Contains(joined, "--allowedTools") {
		t.Errorf("default launch: gate=%v args %q, want no implicit policy", def.Gate, joined)
	}
	_, _, env := launchOf(t, agent.RunOptions{Tools: true, Provider: "qa", WorkDir: t.TempDir(),
		Permissions: agent.Permissions{Deny: []agent.Rule{deny}, Ask: []agent.Rule{ask}}})
	got := envPolicy(t, env)
	if got.Mode != "" || len(got.Allow) != 0 || !slices.Contains(got.Deny, deny) || !slices.Contains(got.Deny, ask) ||
		slices.Contains(got.Deny, agent.Rule{Tool: "Bash", Arg: agent.RedirectFileArg}) {
		t.Errorf("explicit rules policy %+v, want only the dispatch's deny and ask-as-deny", got)
	}
	_, _, env = launchOf(t, agent.RunOptions{Tools: true, ReadOnly: true, Provider: "qa", WorkDir: t.TempDir()})
	if ro := envPolicy(t, env); ro.Mode != "plan" || !slices.Contains(ro.Deny, agent.Rule{Tool: "Bash", Arg: agent.RedirectFileArg}) {
		t.Errorf("read-only policy %+v, want its plan-mode gate policy", ro)
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
	if err := refuseUngatedBypass(context.Background(), []string{"--permission-mode", "bypassPermissions"}, agent.RunOptions{Tools: true}); err == nil {
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

type gateRow struct {
	name        string
	user        agent.Permissions
	tool        string
	input       map[string]any
	block       bool
	reason, not string
}

func gateWrite(path string) map[string]any { return map[string]any{"file_path": path, "content": "ok"} }

func gateBash(cmd string) map[string]any { return map[string]any{"command": cmd} }

// runGateRows runs each row through the real gate under the policy readOnlyGatePolicy builds from the row's user rules.
func runGateRows(t *testing.T, wd string, cases []gateRow) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := gateCall(t, readOnlyGatePolicy(c.user), wd, c.tool, c.input)
			if (res.Decision == core.Block) != c.block || !strings.Contains(res.Reason, c.reason) ||
				c.not != "" && strings.Contains(res.Reason, c.not) {
				t.Fatalf("decision %v reason %q, want block=%v naming %q and not %q", res.Decision, res.Reason, c.block,
					c.reason, c.not)
			}
		})
	}
}

// A --read-only claude run's gate policy: reads and the shared shell list run; builds, writes and redirects do not.
func TestReadOnlyGatePolicy(t *testing.T) {
	wd := t.TempDir()
	user := agent.Permissions{Allow: []agent.Rule{agent.ReadOnlyAllow}, Deny: agent.ReadOnlyDeny}
	read := map[string]any{"file_path": filepath.Join(wd, "a.go")}
	row := func(name, tool string, in map[string]any, block bool) gateRow {
		return gateRow{name: name, user: user, tool: tool, input: in, block: block}
	}
	runGateRows(t, wd, []gateRow{
		row("Read", "Read", read, false),
		row("git log", "Bash", gateBash("git log -1"), false),
		row("git diff piped", "Bash", gateBash("git diff | head -5"), false),
		row("go test", "Bash", gateBash("go test ./..."), true),
		row("Edit", "Edit", gateWrite(filepath.Join(wd, "a.go")), true),
		row("Write", "Write", gateWrite(filepath.Join(wd, "b.go")), true),
		row("file redirect", "Bash", gateBash("git log -1 > out"), true),
		row("substitution", "Bash", gateBash("cat $(ls)"), true),
		row("memory remember", "Bash", gateBash("claude-code memory remember x"), true),
	})
	if got := gateArgs(agent.RunOptions{ReadOnly: true}); !slices.Contains(got, readOnlyGateTools) || slices.Contains(got, gateTools) {
		t.Errorf("read-only gate tools = %v, want %s", got, readOnlyGateTools)
	}
}

// The allow list matches every spelling the rtk rewrite hook can give its command word, and no other rtk.
func TestGatePolicyAllowsRtkSpellings(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	home := t.TempDir()
	bin := filepath.Join(home, "bin", "rtk")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv(paths.RTKBinaryEnv, bin)
	p := agent.Permissions{Allow: gatewayAllow()}
	for cmd, want := range map[string]agent.Decision{
		"rtk go test ./x": agent.Allow, `"$HOME/bin/rtk" go test ./x`: agent.Allow, bin + " git status": agent.Allow,
		`"$HOME/bin/rtk" read x.go`: agent.Allow, `"$HOME/other/rtk" go test ./x`: agent.Ask, "/tmp/evil/rtk go test": agent.Ask,
		`"$HOME/bin/rtk" curl x`: agent.Ask,
	} {
		if got := p.Decide("Bash", cmd); got != want {
			t.Errorf("%q: got %v, want %v", cmd, got, want)
		}
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

func TestGatePermissionCapabilityRefusal(t *testing.T) {
	hermetic(t, dbxProfiles)
	for _, req := range []agent.RunOptions{
		{Tools: true, Provider: "qa", Prompt: "Run saved Workflow code-workers", Permissions: agent.Permissions{Deny: []agent.Rule{{Tool: "Bash", Arg: "curl *"}}}},
		{Tools: true, Provider: "qa", Prompt: `Run Workflow({scriptPath: "~/.claude/workflows/x.js"}) and print it`, Permissions: agent.Permissions{Deny: []agent.Rule{{Tool: "Bash", Arg: "curl *"}}}},
		{Tools: true, Provider: "qa", Permissions: agent.Permissions{Allow: []agent.Rule{{Tool: "Workflow"}}}},
		{Tools: true, Provider: "qa", Permissions: agent.Permissions{Allow: []agent.Rule{{Tool: "WebFetch"}}}},
		{NoTools: true, Gate: true},
	} {
		ctx, gated := gateOffAnthropic(context.Background(), req)
		_, _, _, err := claudeCmd(ctx, gated, nil, "")
		if err == nil || !strings.Contains(err.Error(), "capability") {
			t.Fatalf("request %+v: want capability refusal before launcher, got %v", req, err)
		}
		if strings.Contains(err.Error(), "Workflow") && !strings.Contains(err.Error(), "native Agent") {
			t.Fatalf("Workflow refusal lacks native Agent remedy: %v", err)
		}
	}
}

// On a gated launch the hook's verdict replays the env policy exactly: the gate blocks iff the policy denies the call.
func TestGatePermissionExactReplay(t *testing.T) {
	hermetic(t, dbxProfiles)
	wd := t.TempDir()
	curl := agent.Rule{Tool: "Bash", Arg: "curl *"}
	for name, c := range map[string]struct {
		req  agent.RunOptions
		deny map[string]bool
	}{
		"explicit deny": {agent.RunOptions{Permissions: agent.Permissions{Deny: []agent.Rule{curl}, Allow: []agent.Rule{curl}}},
			map[string]bool{"curl -s x": true, "ls -l": false}},
		"read-only": {agent.RunOptions{ReadOnly: true},
			map[string]bool{"git log -1": false, "go test ./x": true, "git log -1 > out": true}},
	} {
		t.Run(name, func(t *testing.T) {
			c.req.Tools, c.req.Provider, c.req.WorkDir = true, "qa", wd
			req, _, env := launchOf(t, c.req)
			if !req.Gate {
				t.Fatal("explicit rules or --read-only must gate the launch")
			}
			policy := envPolicy(t, env)
			for cmd, deny := range c.deny {
				blocked := gateCall(t, policy, wd, "Bash", gateBash(cmd)).Decision == core.Block
				if blocked != deny || (policy.Decide("Bash", cmd) == agent.Deny) != deny {
					t.Errorf("%q: gate blocked=%v, policy %v, want deny=%v", cmd, blocked, policy.Decide("Bash", cmd), deny)
				}
			}
		})
	}
}

func TestAsksToRunWorkflow(t *testing.T) {
	for _, c := range []struct {
		brief string
		want  bool
	}{
		{"Run saved Workflow code-workers", true},
		{`Run Workflow({scriptPath: "~/.claude/workflows/x.js"}) and print it`, true},
		{"First fix the lint, then run the saved workflow code-workers.", true},
		{"Invoke the Workflow tool with workflows/code-looper.js", true},
		{"Fix gate.go.\nCall Workflow({name: 'code-looper'}) next.", true},
		{"this replaces the saved Workflow", false},
		{"Do not run a saved Workflow; use the native Agent tool.", false},
		{"The workflow scripts live in workflows/", false},
		{"Never call Workflow({name: 'x'}) from here", false},
		{"Edit workflows/x.js, a saved Workflow script, to fix the lint", false},
	} {
		if got := asksToRunWorkflow(c.brief); got != c.want {
			t.Errorf("asksToRunWorkflow(%q) = %v, want %v", c.brief, got, c.want)
		}
	}
}

func TestGatewayAllowsMergedAgentVerbs(t *testing.T) {
	p := agent.Permissions{Allow: gatewayAllow()}
	for cmd, allowed := range map[string]bool{
		"claude-code agents result -f job": true, "claude-code agents result job --events -f": true,
		"claude-code agents status": true, "claude-code agents status job --agent codex": true,
		"claude-code agents cancel job": true, "claude-code agents cancel --lost": true,
		"claude-code agents tail -f job": true, "claude-code agents reap": true, "curl x": false,
	} {
		if got := p.Decide("Bash", cmd) == agent.Allow; got != allowed {
			t.Errorf("%s: allowed = %v, want %v", cmd, got, allowed)
		}
	}
}

func TestGatewayAllowsDispatchAndWait(t *testing.T) {
	p := agent.Permissions{Allow: gatewayAllow()}
	for _, cmd := range []string{"claude-code agents dispatch --waves plan --background", "claude-code agents wait job --timeout 10m"} {
		if got := p.Decide("Bash", cmd); got != agent.Allow {
			t.Errorf("%s: %v", cmd, got)
		}
	}
}
