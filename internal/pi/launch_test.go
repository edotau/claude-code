package pi

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/rtkwrap"
	"github.com/edotau/claude-code/internal/vault"
)

func interactive(t *testing.T) (env, argv []string) {
	t.Helper()
	env, argv, err := piEngine{}.Wire(t.Context(), testRoute(t, "databricks-claude-sonnet-5-5", true), nil)
	if err != nil {
		t.Fatal(err)
	}
	return env, argv
}

func TestInteractiveWireIsGuardedAndStreamless(t *testing.T) {
	env, argv := interactive(t)
	g, ok := guardFromEnv(env)
	if !ok {
		t.Fatalf("env has no guard: %v", envNames(env))
	}
	if i := slices.Index(argv, "-e"); i < 0 || argv[i+1] != g.Path {
		t.Errorf("argv does not load the guard: %v", argv)
	}
	for _, want := range []string{"--no-builtin-tools", "--no-approve", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "--no-mcp"} {
		if !slices.Contains(argv, want) {
			t.Errorf("argv lacks %s: %v", want, argv)
		}
	}
	if strings.Contains(strings.Join(argv, " "), testCred) {
		t.Error("the token is on argv")
	}
	if !slices.Contains(env, guardToolsEnv+"=read,bash,edit,write,grep,find,ls") || !slices.Contains(env, "HARNESS_PROXY_API_KEY="+testCred) {
		t.Errorf("env = %v", envNames(env))
	}
}

func TestInteractiveWireDropsOnlyStreamAndContextLockdown(t *testing.T) {
	_, argv := interactive(t)
	for _, bad := range []string{"--mode", "--no-context-files", "--no-session", "--approve", "-a"} {
		if slices.Contains(argv, bad) {
			t.Errorf("argv has %s: %v", bad, argv)
		}
	}
}

func TestInteractiveWireRefusesWithoutGuard(t *testing.T) {
	old := writeGuard
	t.Cleanup(func() { writeGuard = old })
	writeGuard = func(string) (Guard, error) { return Guard{}, errors.New("disk full") }
	_, _, err := piEngine{}.Wire(t.Context(), testRoute(t, "databricks-claude-sonnet-5-5", true), nil)
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v, want the guard refusal", err)
	}
	if left, _ := os.ReadDir(RunsDir("dbx")); len(left) != 0 {
		t.Errorf("a refused launch left run dirs: %v", left)
	}
}

func TestPreviewLeavesNoRunDirButLaunchKeepsIt(t *testing.T) {
	route := testRoute(t, "databricks-claude-sonnet-5-5", true)
	if _, _, err := (piEngine{}).Wire(agent.WithDryRun(t.Context()), route, nil); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(RunsDir("dbx")); len(left) != 0 {
		t.Fatalf("a preview left run dirs: %v", left)
	}
	env, _, err := piEngine{}.Wire(t.Context(), route, nil)
	if err != nil {
		t.Fatal(err)
	}
	g, _ := guardFromEnv(env)
	if _, err := os.Stat(g.Path); err != nil {
		t.Errorf("a real launch lost its run dir before exec: %v", err)
	}
}

func TestInteractiveWireRefusesDirect(t *testing.T) {
	if _, _, err := (piEngine{}).Wire(t.Context(), testRoute(t, "databricks-claude-sonnet-5-5", false), nil); err == nil {
		t.Fatal("a direct launch must be refused")
	}
}

func TestExperimentIsOneTrial(t *testing.T) {
	for _, c := range []struct{ codemode, rtk, want string }{
		{"", "", "none"}, {"", "debug", "none"}, {"", " ON ", "rtk-wrap"}, {"on", "", "codemode"}, {"only", "debug", "codemode"},
		{"on", "on", "codemode"},
	} {
		if got := experiment(c.codemode, c.rtk); got != c.want {
			t.Errorf("experiment(%q, %q) = %q, want %q", c.codemode, c.rtk, got, c.want)
		}
	}
}

func TestResolveCodemode(t *testing.T) {
	for name, c := range map[string]struct {
		typed, setting, rtk, want string
		tools                     bool
	}{
		"setting":      {"", "only", "", "only", true},
		"case":         {"", " On ", "", "on", true},
		"typed wins":   {"on", "only", "on", "on", true},
		"no tools":     {"", "only", "", "", false},
		"bad value":    {"", "yes", "", "", true},
		"unset":        {"", "", "", "", true},
		"with rtkwrap": {"", "only", "on", "only", true},
	} {
		t.Setenv(CodemodeEnv, c.setting)
		t.Setenv(rtkwrap.ModeEnv, c.rtk)
		got, err := ResolveCodemode(c.typed, c.tools)
		if got != c.want || err != nil {
			t.Errorf("%s: got %q, %v; want %q", name, got, err, c.want)
		}
	}
}

func TestCheckCodemode(t *testing.T) {
	for name, c := range map[string]struct {
		req agent.RunOptions
		ok  bool
	}{
		"off":          {agent.RunOptions{}, true},
		"on":           {agent.RunOptions{Tools: true, Codemode: "on"}, true},
		"bad mode":     {agent.RunOptions{Tools: true, Codemode: "yes"}, false},
		"no tools":     {agent.RunOptions{Codemode: "only"}, false},
		"with rtkwrap": {agent.RunOptions{Tools: true, Codemode: "on"}, true},
	} {
		if err := checkCodemode(c.req, []string{"RTK_WRAP=on"}); (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", name, err, c.ok)
		}
	}
}

func TestCodemodeLaunchEnvExportsMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shims are unix-only")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(rtkwrap.ModeEnv, "on")
	if err := os.MkdirAll(filepath.Dir(rtkwrap.Binary()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rtkwrap.Binary(), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := codemodeLaunchEnv([]string{rtkwrap.CodemodeEnv + "=stale"}, "only")
	if got := lastEnv(env, rtkwrap.CodemodeEnv); got != "only" {
		t.Errorf("%s = %q, want only", rtkwrap.CodemodeEnv, got)
	}
	if got := lastEnv(env, rtkwrap.ModeEnv); got != "on" || !strings.HasPrefix(lastEnv(env, "PATH"), rtkwrap.Dir()) {
		t.Errorf("RTK_WRAP = %q, PATH = %q", got, lastEnv(env, "PATH"))
	}
	t.Setenv(rtkwrap.ModeEnv, "")
	if got := codemodeLaunchEnv(nil, "on"); lastEnv(got, rtkwrap.ModeEnv) != "" || !strings.HasPrefix(lastEnv(got, "PATH"), rtkwrap.Dir()) {
		t.Errorf("code mode alone: env lacks the shim dir or sets RTK_WRAP: %v", got)
	}
}

func TestWriteCodemodeSettingsKeepsTheRunSettings(t *testing.T) {
	dir := t.TempDir()
	if err := WriteConfig(dir, []byte(`{}`), []byte(`{"defaultModel":"m"}`)); err != nil {
		t.Fatal(err)
	}
	if err := writeCodemodeSettings([]string{agentDirEnv + "=" + dir}, "only"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(dir + "/settings.json")
	if err != nil || !strings.Contains(string(raw), `"mode": "only"`) || !strings.Contains(string(raw), `"defaultModel": "m"`) {
		t.Errorf("settings.json = %s, %v", raw, err)
	}
}

func TestLaunchArgsDropsReadOnly(t *testing.T) {
	got, err := piEngine{}.LaunchArgs("", []string{"--read-only", "-c"})
	if err != nil || !slices.Equal(got, []string{"-c"}) {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestLaunchArgsAllowList(t *testing.T) {
	for _, ok := range [][]string{{"-c"}, {"--resume"}, {"--session", "abc"}, {"-n", "x", "--thinking", "high"}, {"-p", "say hi"}, {"fix it"}} {
		if _, err := (piEngine{}).LaunchArgs("", ok); err != nil {
			t.Errorf("%v refused: %v", ok, err)
		}
	}
	for _, bad := range [][]string{{"-e", "/tmp/x.ts"}, {"--approve"}, {"-a"}, {"--skill", "s"}, {"--tools", "bash"}, {"--model", "m"},
		{"--mode", "rpc"}, {"--", "-e", "x"}, {"@/etc/hosts"}, {"--session=x"}, {"-c", "--no-builtin-tools"}} {
		_, err := (piEngine{}).LaunchArgs("", bad)
		var usage *agent.UsageError
		if !errors.As(err, &usage) || !strings.Contains(err.Error(), "packages=") {
			t.Errorf("%v: err = %v, want a refusal naming the harness way", bad, err)
		}
	}
	st := agent.Settings{Record: vault.AgentRecord{Launch: &vault.AgentLaunch{Args: []string{"-e", "/x.ts"}}}}
	if _, _, _, _, err := (piEngine{}).RenderSettings(t.Context(), st, nil, nil); err == nil {
		t.Error("record launch.args -e reached the TUI argv")
	}
}

func TestHeadlessPromptNeverParsesAsAFlag(t *testing.T) {
	for prompt, want := range map[string]string{"-a": "-a", "--approve": "--approve", "@secret": " @secret", "a@b": "a@b"} {
		p := &agent.CLIPrep{Prompt: prompt}
		piEngine{}.After(p, agent.RunOptions{})
		if n := len(p.Argv); n < 2 || p.Argv[n-2] != "--" || p.Argv[n-1] != want {
			t.Errorf("prompt %q: argv tail %v, want -- %q", prompt, p.Argv, want)
		}
	}
}

func TestInteractiveReadOnlyRegistersOnlyReadTools(t *testing.T) {
	ctx := agent.WithReadOnly(t.Context())
	env, argv, err := piEngine{}.Wire(ctx, testRoute(t, "databricks-claude-sonnet-5-5", true), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env, guardToolsEnv+"=read,grep,find,ls") || slices.Contains(argv, "--tools") {
		t.Errorf("env = %v argv = %v", env, argv)
	}
}

func TestInteractivePackagesAfterGuardAndPreviewInstallsNothing(t *testing.T) {
	route := testRoute(t, "databricks-claude-sonnet-5-5", true)
	pkg := skillPkg(t)
	setPackages(t, pkg+",npm:foo@1.0.0", "")
	_, argv, err := piEngine{}.Wire(agent.WithDryRun(t.Context()), route, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(packageStore()); err == nil {
		t.Error("a preview created the package store")
	}
	if i := slices.Index(argv, "--no-builtin-tools"); i < 0 || !slices.Equal(argv[i+1:i+3], []string{"-e", pkg}) || slices.Index(argv, "-e") > i {
		t.Errorf("argv = %v, want the guard's -e first and the package -e after it", argv)
	}
}
