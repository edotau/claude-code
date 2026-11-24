package pi

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
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
	for name, c := range map[string]struct {
		req  agent.RunOptions
		want []string
	}{
		"no tools": {agent.RunOptions{}, []string{"--no-tools"}},
		"tools":    {agent.RunOptions{Tools: true}, []string{"-e", g.Path, "--no-builtin-tools"}},
		"codemode": {agent.RunOptions{Tools: true, Codemode: "only"},
			[]string{"-e", g.Path, "--no-builtin-tools", "-e", "builtin:codemode", "--tools", "+codemode"}},
	} {
		got, err := toolsArgv(c.req, env)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, %v; want %v", name, got, err, c.want)
		}
	}
}

// No guard, no tools: without the guard env every tools request fails and yields no argv at all.
func TestToolsArgvWithoutGuardOffersNoTools(t *testing.T) {
	_, g := guardedEnv(t)
	for name, env := range map[string][]string{"empty": nil, "dir only": {agentDirEnv + "=/x"}, "no nonce": {agentDirEnv + "=/x", nonceFileEnv + "=" + g.NonceFile}} {
		if got, err := toolsArgv(agent.RunOptions{Tools: true}, env); err == nil || got != nil {
			t.Errorf("%s: got %v, %v; want a refusal", name, got, err)
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
	for name, c := range map[string]struct {
		req  agent.RunOptions
		want string
	}{
		"tools":             {agent.RunOptions{Tools: true}, ""},
		"nonce env":         {agent.RunOptions{Env: []string{"HARNESS_PI_GUARD_NONCE=x"}}, "HARNESS_PI_GUARD_NONCE"},
		"agent dir env":     {agent.RunOptions{Env: []string{agentDirEnv + "=/x"}}, agentDirEnv},
		"hook skip env":     {agent.RunOptions{Env: []string{"HARNESS_HOOK_SKIP=safety-scan"}}, "HARNESS_HOOK_SKIP"},
		"hook env, cased":   {agent.RunOptions{Env: []string{"harness_hook_agent=x"}}, "harness_hook_agent"},
		"agent dir, lower":  {agent.RunOptions{Env: []string{strings.ToLower(agentDirEnv) + "=/x"}}, strings.ToLower(agentDirEnv)},
		"overlay lower":     {agent.RunOptions{OverlaySet: []string{"harness_pi_guard_nonce"}}, "harness_pi_guard_nonce"},
		"overlay hook set":  {agent.RunOptions{OverlaySet: []string{"HARNESS_HOOK_SKIP"}}, "HARNESS_HOOK_SKIP"},
		"overlay agent dir": {agent.RunOptions{OverlaySet: []string{agentDirEnv}}, agentDirEnv},
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
	if got := p.Argv[len(p.Argv)-1]; got != "do it" || !slices.Contains(p.Argv, "--no-builtin-tools") || !slices.Contains(p.Argv, g.Path) {
		t.Errorf("argv = %v", p.Argv)
	}
	if !slices.Contains(p.Env, guardToolsEnv+"=read,bash,edit,write,grep,find,ls,gate,handback,report") {
		t.Errorf("env = %v", p.Env)
	}
	if slices.Contains(p.Argv, "--tools") {
		t.Errorf("--tools would re-enable builtin tools: %v", p.Argv)
	}
}

func TestAfterPassesVerifyToGuard(t *testing.T) {
	for _, verify := range []string{"go vet ./internal/pi", ""} { // empty still set: an inherited line never applies
		env, _ := guardedEnv(t)
		p := &agent.CLIPrep{Env: env, Prompt: "do it"}
		piEngine{}.After(p, agent.RunOptions{Tools: true, Verify: verify})
		if !slices.Contains(p.Env, guardVerifyEnv+"="+verify) {
			t.Errorf("verify %q: env = %v", verify, p.Env)
		}
	}
	if src := assets.PiGuardSource(); !strings.Contains(src, guardVerifyEnv) {
		t.Error("the guard must read the verify env")
	}
}

func TestAfterSystemPrompt(t *testing.T) {
	env, _ := guardedEnv(t)
	for name, c := range map[string]struct {
		req  agent.RunOptions
		want string
	}{
		"tools run gets the harness prompt": {agent.RunOptions{Tools: true}, assets.PiSystemPrompt()},
		"caller's prompt wins":              {agent.RunOptions{Tools: true, SystemPrompt: "mine"}, "mine"},
		"no tools run names no tools":       {agent.RunOptions{}, ""},
		"codemode hides the tools":          {agent.RunOptions{Tools: true, Codemode: "only"}, ""},
		"codemode on keeps the tools":       {agent.RunOptions{Tools: true, Codemode: "on"}, assets.PiSystemPrompt()},
	} {
		p := &agent.CLIPrep{Env: env, Prompt: "x"}
		piEngine{}.After(p, c.req)
		i := slices.Index(p.Argv, "--system-prompt")
		if (c.want == "") != (i < 0) || i >= 0 && p.Argv[i+1] != c.want {
			t.Errorf("%s: argv = %v", name, p.Argv)
		}
	}
	for tool := range strings.SplitSeq(guardTools, ",") {
		if !strings.Contains(assets.PiSystemPrompt(), tool) {
			t.Errorf("prompt does not name registered tool %s", tool)
		}
	}
}

func TestAfterDeliversAttachmentsBeforePrompt(t *testing.T) {
	p := &agent.CLIPrep{Argv: []string{"--mode", "json"}, Prompt: "@x"}
	piEngine{}.After(p, agent.RunOptions{Attach: []string{"/a/f.txt"}})
	if n := len(p.Argv); p.Argv[n-3] != "--" || p.Argv[n-2] != "@/a/f.txt" || p.Argv[n-1] != " @x" {
		t.Errorf("argv = %v", p.Argv)
	}
	if _, err := (piEngine{}).Attach("t", []string{"/nonexistent/f.txt"}, false); !errors.Is(err, agent.ErrBadAttach) {
		t.Errorf("missing attach err = %v", err)
	}
}

func TestWithoutHookControl(t *testing.T) {
	got := withoutHookControl([]string{"A=1", "HARNESS_HOOK_SKIP=all", "harness_hook_agent=x", "HARNESS_HOOKS=keep", "HARNESS_DISPATCH_ID=x"})
	if want := []string{"A=1", "HARNESS_HOOKS=keep", "HARNESS_DISPATCH_ID=x"}; !slices.Equal(got, want) {
		t.Errorf("env = %v, want %v", got, want)
	}
}

func TestRowRegistered(t *testing.T) {
	s, ok := agent.LookupSchema("pi")
	c := s.CLI.Caps
	if !ok || s.CLI.Binary != "pi" || c.WorkDir != agent.Emulated {
		t.Errorf("pi row = %+v (found %v)", s.CLI, ok)
	}
	if err := agent.CheckCapabilities("pi", agent.RunOptions{Tools: true}); err != nil {
		t.Errorf("pi refuses a tools run: %v", err)
	}
	if agent.IdleLimitFor("pi") == 0 {
		t.Error("pi has no idle limit")
	}
}

func TestChildCmdCarriesNoHookControl(t *testing.T) {
	t.Setenv("HARNESS_HOOK_SKIP", "safety-scan")
	t.Setenv("HARNESS_KEEP_ENV", "HARNESS_HOOK_SKIP")
	cmd, _ := childCmd(t.Context(), "pi", agent.CLIPrep{Env: []string{"HARNESS_HOOK_AGENT=x", "KEEP=1"}}, agent.RunOptions{KeepEnv: []string{"HARNESS_HOOK_SKIP"}})
	for _, kv := range cmd.Env {
		if strings.HasPrefix(strings.ToUpper(kv), "HARNESS_HOOK_") {
			t.Errorf("child env holds %s", kv)
		}
	}
	if !slices.Contains(cmd.Env, "KEEP=1") {
		t.Error("the launch's own env was dropped")
	}
}

func TestToolIdleResumesOnlyWhenNoToolRuns(t *testing.T) {
	idle := agent.NewIdleTimeout("pi", 150*time.Millisecond, nil)
	ti := newToolIdle(idle)
	for _, ev := range []piEvent{{Type: "tool_execution_start", ToolCallID: "bash"}, {Type: "tool_execution_start", ToolCallID: "read"},
		{Type: "tool_execution_end", ToolCallID: "read"}, {Type: "tool_execution_end", ToolCallID: "stray"}, {Type: "message_update"}} {
		ti.beat(ev)
	}
	time.Sleep(400 * time.Millisecond)
	if err := idle.Err(); err != nil {
		t.Fatalf("idle tripped while bash still ran: %v", err)
	}
	ti.beat(piEvent{Type: "tool_execution_end", ToolCallID: "bash"})
	time.Sleep(400 * time.Millisecond)
	if idle.Err() == nil {
		t.Fatal("idle never resumed after the last tool ended")
	}
}

func TestToolIdleHoldsCompactionAndRetryWaits(t *testing.T) {
	const (
		limit = 60 * time.Millisecond
		wait  = 3 * limit
	)
	run := func(maxHold time.Duration, evs ...piEvent) (*agent.IdleTimeout, *toolIdle) {
		idle := agent.NewIdleTimeout("pi", limit, nil)
		ti := newToolIdle(idle)
		ti.maxHold = maxHold
		for _, ev := range evs {
			ti.beat(ev)
		}
		return idle, ti
	}
	idle, ti := run(time.Minute, piEvent{Type: "compaction_start"})
	time.Sleep(3 * limit)
	if err := idle.Err(); err != nil {
		t.Fatalf("killed during compaction: %v", err)
	}
	ti.beat(piEvent{Type: "compaction_end"})
	time.Sleep(3 * limit)
	if idle.Err() == nil {
		t.Error("silence after compaction_end was not killed")
	}
	idle, _ = run(time.Minute, piEvent{Type: "auto_retry_start", DelayMs: int(wait.Milliseconds())})
	time.Sleep(wait - limit)
	if err := idle.Err(); err != nil {
		t.Fatalf("killed inside the announced retry wait: %v", err)
	}
	time.Sleep(wait)
	if idle.Err() == nil {
		t.Error("not killed after the retry wait plus the limit")
	}
	idle, _ = run(time.Minute, piEvent{Type: "summarization_retry_scheduled", DelayMs: int(time.Minute.Milliseconds())},
		piEvent{Type: "summarization_retry_finished"})
	time.Sleep(wait)
	if idle.Err() == nil {
		t.Error("an early summarization_retry_finished left the watch held")
	}
	_, ti = run(time.Minute, piEvent{Type: "compaction_start"})
	ti.stop()
	if len(ti.holds) != 0 {
		t.Error("stop left a hold timer open")
	}
	idle, _ = run(wait, piEvent{Type: "compaction_start"})
	time.Sleep(wait + 3*limit)
	if idle.Err() == nil {
		t.Error("a never-ending compaction held the watch past its bound")
	}
}

// setPackages writes the Pi record's pin extras into the test vault.
func setPackages(t *testing.T, packages, approved string) {
	t.Helper()
	rec := vault.AgentRecord{Extras: map[string]string{extraPackages: packages, extraPackagesApproved: approved}}
	if err := vault.Update(func(tx *vault.Tx) error { return tx.PutAgentRecords(map[string]vault.AgentRecord{"pi": rec}) }); err != nil {
		t.Fatal(err)
	}
}

// skillPkg is a local package with one skill and no code.
func skillPkg(t *testing.T) string {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "package.json"), `{"name":"s","version":"1.0.0","pi":{"skills":["./skills"]}}`)
	writeFile(t, filepath.Join(dir, "skills", "x", "SKILL.md"), "x")
	return dir
}

// The guard's -e must be Pi's first: it keeps the first registration of a tool name.
func TestHeadlessPackagesLoadAfterGuard(t *testing.T) {
	route := testRoute(t, "databricks-claude-sonnet-5-5", true)
	pkg := skillPkg(t)
	setPackages(t, pkg, "")
	env, argv, err := piEngine{}.Wire(t.Context(), route, &agent.Headless{Tools: true})
	if err != nil {
		t.Fatal(err)
	}
	p := &agent.CLIPrep{Argv: argv, Env: env, Prompt: "go"}
	piEngine{}.After(p, agent.RunOptions{Tools: true})
	g, _ := guardFromEnv(p.Env)
	var es []string
	for i, a := range p.Argv {
		if a == "-e" {
			es = append(es, p.Argv[i+1])
		}
	}
	if !slices.Equal(es, []string{g.Path, pkg}) {
		t.Errorf("-e order = %v, want guard %s then %s", es, g.Path, pkg)
	}
	if slices.ContainsFunc(p.Env, func(kv string) bool { return strings.HasPrefix(kv, packageDirsEnv+"=") }) {
		t.Errorf("Pi's env carries the package dirs: %v", p.Env)
	}
}

func TestSyncErrorFailsLaunchAndRemovesRunDir(t *testing.T) {
	route := testRoute(t, "databricks-claude-sonnet-5-5", true)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "package.json"), `{"name":"c","pi":{"extensions":["./e.ts"]}}`)
	setPackages(t, dir, "")
	_, _, err := piEngine{}.Wire(t.Context(), route, &agent.Headless{Tools: true})
	if err == nil || !strings.Contains(err.Error(), "packages_approved="+dir) {
		t.Fatalf("unapproved code: err = %v, want the approving command", err)
	}
	if left, _ := os.ReadDir(RunsDir("dbx")); len(left) != 0 {
		t.Errorf("a refused launch left run dirs: %v", left)
	}
	setPackages(t, dir, dir)
	if _, _, err := (piEngine{}).Wire(t.Context(), route, &agent.Headless{Tools: true}); err != nil {
		t.Errorf("approved: %v", err)
	}
}

func TestEmptyPackageListSyncsNothing(t *testing.T) {
	route := testRoute(t, "databricks-claude-sonnet-5-5", true)
	if _, _, err := (piEngine{}).Wire(t.Context(), route, &agent.Headless{Tools: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(packageStore()); err == nil {
		t.Error("an empty list created the package store")
	}
}

func TestWebHint(t *testing.T) {
	if webHint(false) != "" || !strings.Contains(webHint(true), "claude-code network websearch") {
		t.Errorf("webHint = %q / %q", webHint(false), webHint(true))
	}
}
