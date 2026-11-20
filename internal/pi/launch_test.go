package pi

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
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

func TestLaunchArgsDropsReadOnly(t *testing.T) {
	got, err := piEngine{}.LaunchArgs("", []string{"--read-only", "-c"})
	if err != nil || !slices.Equal(got, []string{"-c"}) {
		t.Errorf("got %v, %v", got, err)
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
