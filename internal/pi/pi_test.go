package pi

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const testCred = "session-token-xyz"

func testRoute(t *testing.T, model string, routed bool) agent.WireRoute {
	t.Helper()
	p := testProvider(t)
	return agent.WireRoute{Provider: p, Dialect: providers.ProtocolOpenAI, Base: "http://127.0.0.1:1/p/dbx/openai",
		Model: model, Cred: testCred, Routed: routed}
}

func TestWireArgvAndEnv(t *testing.T) {
	env, argv, err := piEngine{}.Wire(t.Context(), testRoute(t, "databricks-claude-sonnet-5-5", true), &agent.Headless{Effort: "high", Tools: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--mode", "json", "--no-mcp", "--no-context-files", "--no-approve", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "--model", "dbx-claude/databricks-claude-sonnet-5-5", "--thinking", "high", "--no-session"}
	if !slices.Equal(argv, want) {
		t.Errorf("argv = %v, want %v", argv, want)
	}
	wantEnv := []string{"PI_CODING_AGENT_DIR", "HARNESS_PROXY_API_KEY", nonceFileEnv, nonceValueEnv}
	if !slices.Equal(envNames(env), wantEnv) || env[1] != "HARNESS_PROXY_API_KEY="+testCred {
		t.Errorf("env names = %v, want %v", envNames(env), wantEnv)
	}
}

// A project's .pi (extensions, packages) must never load: stage 1 is read-only only while no project code runs.
func TestWireLoadsNoProjectCode(t *testing.T) {
	_, argv, err := piEngine{}.Wire(t.Context(), testRoute(t, "databricks-claude-sonnet-5-5", true), &agent.Headless{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"--no-approve", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "--no-mcp"} {
		if !slices.Contains(argv, f) {
			t.Errorf("argv lacks %s: %v", f, argv)
		}
	}
	for _, bad := range []string{"--approve", "-a", "-e", "--extension", "--skill", "--prompt-template", "--theme"} {
		if slices.Contains(argv, bad) {
			t.Errorf("argv has %s: %v", bad, argv)
		}
	}
}

func TestWirePrepErrorLeavesNoRunDir(t *testing.T) {
	vaulttest.Use(t)
	_, _, err := piEngine{}.Wire(t.Context(), testRoute(t, "databricks-claude-sonnet-5-5", true), &agent.Headless{Effort: "bogus"})
	if err == nil || !strings.Contains(err.Error(), "effort") {
		t.Fatalf("err = %v, want an effort refusal", err)
	}
	if left, _ := os.ReadDir(RunsDir("dbx")); len(left) != 0 {
		t.Errorf("a refused dispatch left run dirs: %v", left)
	}
}

func envNames(env []string) []string {
	var n []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		n = append(n, k)
	}
	return n
}

func TestWireRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		model  string
		routed bool
		want   string
	}{"off-menu model": {"no-such-model", true, "not on the"}, "direct": {"databricks-claude-sonnet-5-5", false, "drop --direct"}} {
		_, _, err := piEngine{}.Wire(t.Context(), testRoute(t, c.model, c.routed), &agent.Headless{})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

// guardedEnv is the env Wire writes for a tools run: the agent dir plus the guard's nonce env.
func guardedEnv(t *testing.T) (env []string, g Guard) {
	t.Helper()
	dir := t.TempDir()
	g, err := WriteGuard(dir)
	if err != nil {
		t.Fatal(err)
	}
	return append([]string{agentDirEnv + "=" + dir}, g.Env()...), g
}

func TestToolsArgv(t *testing.T) {
	env, g := guardedEnv(t)
	plan := agent.Permissions{Mode: agent.PermPlan}
	for name, c := range map[string]struct {
		req  agent.RunOptions
		want []string
	}{
		"no tools":    {agent.RunOptions{}, []string{"--no-tools"}},
		"read-only":   {agent.RunOptions{Tools: true, ReadOnly: true}, []string{"-e", g.Path, "--tools", "read,grep,find,ls"}},
		"plan mode":   {agent.RunOptions{Tools: true, Permissions: plan}, []string{"-e", g.Path, "--tools", "read,grep,find,ls"}},
		"write tools": {agent.RunOptions{Tools: true}, []string{"-e", g.Path, "--tools", "read,bash,edit,write,grep,find,ls"}},
		"file deny":   {agent.RunOptions{Tools: true, Permissions: agent.Permissions{Deny: []agent.Rule{{Tool: "Edit", Arg: "x"}}}}, []string{"-e", g.Path, "--tools", "read,edit,write,grep,find,ls"}},
		"bash deny":   {agent.RunOptions{Tools: true, Permissions: agent.Permissions{Deny: []agent.Rule{{Tool: "Bash", Arg: "git push*"}}}}, []string{"-e", g.Path, "--tools", "read,bash,edit,write,grep,find,ls"}},
	} {
		got, err := toolsArgv(c.req, env)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, %v; want %v", name, got, err, c.want)
		}
		if slices.ContainsFunc(got, func(a string) bool { return strings.Contains(a, "powershell") }) {
			t.Errorf("%s: powershell offered: %v", name, got)
		}
	}
}

// No guard, no tools: without the guard env every tools request fails and yields no argv at all.
func TestToolsArgvWithoutGuardOffersNoTools(t *testing.T) {
	_, g := guardedEnv(t)
	for name, env := range map[string][]string{"empty": nil, "dir only": {agentDirEnv + "=/x"}, "no nonce": {agentDirEnv + "=/x", nonceFileEnv + "=" + g.NonceFile}} {
		for _, ro := range []bool{false, true} {
			if got, err := toolsArgv(agent.RunOptions{Tools: true, ReadOnly: ro}, env); err == nil || got != nil {
				t.Errorf("%s ro=%v: got %v, %v; want a refusal", name, ro, got, err)
			}
		}
	}
}

func TestWireToolsRunWritesGuard(t *testing.T) {
	env, argv, err := piEngine{}.Wire(t.Context(), testRoute(t, "databricks-claude-sonnet-5-5", true), &agent.Headless{Tools: true})
	if err != nil {
		t.Fatal(err)
	}
	g, ok := guardFromEnv(env)
	if !ok {
		t.Fatalf("a tools run's env has no guard: %v", envNames(env))
	}
	if _, err := os.Stat(g.Path); err != nil {
		t.Errorf("guard not written: %v", err)
	}
	if slices.Contains(argv, "-e") {
		t.Errorf("Wire must leave -e to toolsArgv: %v", argv)
	}
	envNo, _, err := piEngine{}.Wire(t.Context(), testRoute(t, "databricks-claude-sonnet-5-5", true), &agent.Headless{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := guardFromEnv(envNo); ok {
		t.Error("a no-tools run wrote a guard")
	}
}

func TestGuardWatchKillsWhenNotReady(t *testing.T) {
	env, g := guardedEnv(t)
	killed := false
	w := newGuardWatch("pi", true, env, &exec.Cmd{Cancel: func() error { killed = true; return nil }})
	err := w.check()
	if err == nil || !killed || !strings.Contains(err.Error(), "pi guard did not load; run refused") || w.cause() == nil {
		t.Fatalf("err = %v killed = %v; want a refusal and a kill", err, killed)
	}
	// ready: the extension wrote this run's nonce
	if err := os.WriteFile(g.NonceFile, []byte(g.Nonce), 0o600); err != nil {
		t.Fatal(err)
	}
	killed = false
	if w = newGuardWatch("pi", true, env, &exec.Cmd{Cancel: func() error { killed = true; return nil }}); w.check() != nil || killed {
		t.Errorf("a ready guard was refused (killed=%v)", killed)
	}
	// no tools: nothing to prove
	if w = newGuardWatch("pi", false, nil, nil); w.check() != nil {
		t.Error("a no-tools run was checked")
	}
}

func TestRefuseRequest(t *testing.T) {
	deny := agent.Permissions{Deny: []agent.Rule{{Tool: "Edit", Arg: "secret.txt"}}, Allow: []agent.Rule{{Tool: "Bash", Arg: "ls"}}, Mode: agent.PermPlan}
	for name, c := range map[string]struct {
		req  agent.RunOptions
		want string
	}{
		"deny, allow, plan": {agent.RunOptions{Tools: true, Permissions: deny}, ""},
		"ask rule":          {agent.RunOptions{Tools: true, Permissions: agent.Permissions{Ask: []agent.Rule{{Tool: "Bash", Arg: "git push"}}}}, "ask rule Bash(git push)"},
		"nonce env":         {agent.RunOptions{Env: []string{"HARNESS_PI_GUARD_NONCE=x"}}, "HARNESS_PI_GUARD_NONCE"},
		"agent dir env":     {agent.RunOptions{Env: []string{agentDirEnv + "=/x"}}, agentDirEnv},
		"policy env":        {agent.RunOptions{Env: []string{agent.DispatchPermissionsEnv + "={}"}}, agent.DispatchPermissionsEnv},
		"other env":         {agent.RunOptions{Env: []string{"FOO=1"}}, ""},
	} {
		err := refuseRequest(c.req)
		if (c.want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

func TestAfterPutsPromptLast(t *testing.T) {
	env, g := guardedEnv(t)
	p := &agent.CLIPrep{Argv: []string{"--mode", "json"}, Env: env, Prompt: "do it"}
	piEngine{}.After(p, agent.RunOptions{Tools: true})
	if got := p.Argv[len(p.Argv)-1]; got != "do it" || !slices.Contains(p.Argv, "read,bash,edit,write,grep,find,ls") || !slices.Contains(p.Argv, g.Path) {
		t.Errorf("argv = %v", p.Argv)
	}
}

func TestRowRegistered(t *testing.T) {
	s, ok := agent.LookupSchema("pi")
	c := s.CLI.Caps
	if !ok || s.CLI.Binary != "pi" || c.ReadOnly != agent.Native || c.Permissions != agent.Native || c.WorkDir != agent.Emulated {
		t.Errorf("pi row = %+v (found %v)", s.CLI, ok)
	}
	for _, req := range []agent.RunOptions{{Tools: true, Permissions: agent.Permissions{Deny: []agent.Rule{{Tool: "Edit", Arg: "x"}}}},
		{Tools: true, Permissions: agent.Permissions{Mode: agent.PermPlan}}} {
		if err := agent.CheckCapabilities("pi", req); err != nil {
			t.Errorf("pi refuses a rule its guard enforces: %v", err)
		}
	}
	if agent.IdleLimitFor("pi") == 0 {
		t.Error("pi has no idle limit")
	}
}
