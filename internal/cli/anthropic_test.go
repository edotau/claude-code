package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const cliFakeToken = "sk" + "-ant-oat01-FAKEFAKEFAKEFAKEFAKEFAKE0123456789"

func TestCmdAnthropicRefusalsDoNotEchoSecrets(t *testing.T) {
	vaulttest.UseFreshKey(t)
	cases := [][]string{
		{"--oauth=" + cliFakeToken},
		{"--x-api-key=" + cliFakeToken},
		{"--oauth", "--x-api-key"},
		{"--x-api-key", "--profile", "anthropic"},
		{"--oauth=MISSING_KEY_FOR_TEST", "--print-env"},
	}
	for _, args := range cases {
		code, out, errOut := hosttest.CaptureOutput(t, func() int { return cmdAnthropic(args) })
		if code == 0 {
			t.Errorf("%q: exit 0", args)
		}
		if strings.Contains(out+errOut, "FAKEFAKE") {
			t.Errorf("%q echoed the secret: %q", args, out+errOut)
		}
	}
}

func TestAnthropicHelpMentionsFlags(t *testing.T) {
	var b strings.Builder
	anthropicFlags(&b).Usage()
	for _, want := range []string{"--oauth", "--x-api-key", "--model"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("help lacks %s:\n%s", want, b.String())
		}
	}
}

// A non-leading --help reaches claude; --print-env is the dry exec, since a real one replaces the test binary.
func TestAnthropicPassesSubcommandHelp(t *testing.T) {
	vaulttest.UseFreshKey(t)
	pathstest.FakeNativeClaude(t, "exit 99")
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdAnthropic([]string{"mcp", "--help", "--print-env"})
	})
	if code != 0 || !strings.Contains(stdout, " mcp --help\n") {
		t.Fatalf("subcommand help: exit %d, stdout %q, stderr %q; want vendor MCP help", code, stdout, stderr)
	}
	code, stdout, stderr = hosttest.CaptureOutput(t, func() int { return cmdAnthropic([]string{"--help"}) })
	if code != 0 || !strings.Contains(stdout, "--oauth") || strings.Contains(stdout, "argv ") {
		t.Fatalf("wrapper help control: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestClaudeCatalogHelpStaysInHarness(t *testing.T) {
	t.Setenv("HARNESS_AGENT_BIN_DIR", t.TempDir()) // no claude binary there: a pass-through would fail
	code, out, errOut := hosttest.CaptureOutput(t, func() int { return cmdClaude([]string{"catalog", "-h"}) })
	if got := out + errOut; code != 0 || !strings.Contains(got, "usage: claude-code claude catalog") {
		t.Fatalf("code %d, output %q", code, got)
	}
}

// Invariant: a typed `<profile>:<model>` is cast at launch input; no launch surface carries a prefix, legacy pins included.
func TestShorthandNeverLeavesTheHarness(t *testing.T) {
	seedAltRows(t)
	for _, args := range [][]string{{"claude", "--model", "opus"}, {"claude", "--model", "alt2:sonnet"},
		{"copilot", "--model", "alt2:opus"}, {"opencode", "--model", "alt2:opus"}, {"pi", "--model", "alt2:opus"},
		{"claude", "--profile", "alt2", "--model", "sonnet"}} {
		code, out, errOut := hosttest.CaptureOutput(t, func() int { return Run(append([]string{"claude-code"}, append(args, "--print-env")...)) })
		if code != 0 || !strings.Contains(out+errOut, "alt") {
			t.Fatalf("%v: exit %d, no plan: %s %s", args, code, out, errOut)
		}
		if strings.Contains(out, "alt2:") || strings.Contains(out, "alt:") {
			t.Errorf("%v: a launch surface (env, argv or settings) carries a prefixed model:\n%s", args, out)
		}
	}
}

// seedAltRows registers anthropic rows alt (default) and alt2, a cross-provider haiku pin and stub agent binaries.
func seedAltRows(t *testing.T) {
	t.Helper()
	vaulttest.UseFreshKey(t)
	dir := cfgDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ALT_KEY", "test-key")
	t.Setenv(providers.EnvSessionProfile, "")
	stubs, native := filepath.Join(dir, "bin"), t.TempDir() // the harness's agent bin; native claude lives outside it
	if err := os.MkdirAll(stubs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(stubs, "copilot"), filepath.Join(stubs, "opencode"), filepath.Join(stubs, "pi"),
		filepath.Join(native, "claude")} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", native+string(os.PathListSeparator)+os.Getenv("PATH"))
	row := func(url string) string {
		return `{"protocol":"anthropic","base_url":"` + url + `","auth":{"type":"key","header":"x-api-key","env":"ALT_KEY"},` +
			`"models":{"opus":"claude-opus-5-5","sonnet":"claude-sonnet-5-5","haiku":"claude-haiku-5-5"}}`
	}
	vaulttest.Put(t, vault.Overlay, "providers", `{"default":"alt","providers":{"anthropic":{"alt":`+
		row("https://alt.example.com")+`,"alt2":`+row("https://alt2.example.com")+`}}}`)
	vaulttest.SetRecord(t, "claude", "aliases.haiku", "alt2:claude-haiku-5-5") // a stored cross-provider pin (D5)
}
