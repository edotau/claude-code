package cli

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const cliFakeToken = "sk-ant-oat01-FAKEFAKEFAKEFAKEFAKEFAKE0123456789"

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
	pathstest.FakeVendor(t, "claude", "exit 99")
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdAnthropic([]string{"mcp", "--help", "--print-env"})
	})
	if code != 0 || !strings.Contains(stdout, "argv     mcp --help\n") {
		t.Fatalf("subcommand help: exit %d, stdout %q, stderr %q; want vendor MCP help", code, stdout, stderr)
	}
	code, stdout, stderr = hosttest.CaptureOutput(t, func() int { return cmdAnthropic([]string{"--help"}) })
	if code != 0 || !strings.Contains(stdout, "--oauth") || strings.Contains(stdout, "argv ") {
		t.Fatalf("wrapper help control: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}
