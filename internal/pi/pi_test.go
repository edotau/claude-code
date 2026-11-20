package pi

import (
	"os"
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
	if len(env) != 2 || !strings.HasPrefix(env[0], "PI_CODING_AGENT_DIR=") || env[1] != "HARNESS_PROXY_API_KEY="+testCred {
		t.Errorf("env names = %v, want PI_CODING_AGENT_DIR and HARNESS_PROXY_API_KEY only", envNames(env))
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

func TestToolsArgv(t *testing.T) {
	for name, c := range map[string]struct {
		req  agent.RunOptions
		want []string
		bad  bool
	}{
		"no tools":    {agent.RunOptions{}, []string{"--no-tools"}, false},
		"read-only":   {agent.RunOptions{Tools: true, ReadOnly: true}, []string{"--tools", "read,grep,find,ls"}, false},
		"write tools": {agent.RunOptions{Tools: true}, nil, true},
	} {
		got, err := toolsArgv(c.req)
		if (err != nil) != c.bad || !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, %v", name, got, err)
		}
	}
}

func TestAfterPutsPromptLast(t *testing.T) {
	p := &agent.CLIPrep{Argv: []string{"--mode", "json"}, Prompt: "do it"}
	piEngine{}.After(p, agent.RunOptions{Tools: true, ReadOnly: true})
	if got := p.Argv[len(p.Argv)-1]; got != "do it" || !slices.Contains(p.Argv, "read,grep,find,ls") {
		t.Errorf("argv = %v", p.Argv)
	}
}

func TestRowRegistered(t *testing.T) {
	s, ok := agent.LookupSchema("pi")
	c := s.CLI.Caps
	if !ok || s.CLI.Binary != "pi" || c.ReadOnly != agent.Native || c.Permissions != agent.None || c.WorkDir != agent.Emulated {
		t.Errorf("pi row = %+v (found %v)", s.CLI, ok)
	}
	if agent.IdleLimitFor("pi") == 0 {
		t.Error("pi has no idle limit")
	}
}
