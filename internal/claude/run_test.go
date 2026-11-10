package claude

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/client"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
)

func TestTakeAsBeatsProfileIdentity(t *testing.T) {
	as, rest, err := providers.TakeAs([]string{"--model", "x", "--as", "sp", "--", "--as", "user"})
	if err != nil || as != providers.IdentitySP {
		t.Fatalf("as=%q err=%v", as, err)
	}
	if len(rest) != 5 || rest[3] != "--as" {
		t.Fatalf("rest=%v", rest)
	}
	// the flag wins over a profile set to user
	if got, _ := providers.ResolveIdentity(providers.IdentityUser, as); got != providers.IdentitySP {
		t.Fatalf("resolved %q", got)
	}
	if _, _, err := providers.TakeAs([]string{"--as=bogus"}); err == nil {
		t.Fatal("bogus accepted")
	}
	if err := client.ValidateAs("sp", "codex"); err == nil {
		t.Fatal("--as accepted for codex")
	}
}

// `<alias> --help` runs claude's own help; a profile with no model for the alias must not refuse it.
func TestLaunchClaudeHelpSkipsModelLookup(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	pathstest.FakeVendor(t, "claude", `[ "$1" = --help ] && echo FAKE-CLAUDE-HELP`)
	code, out, errOut := hosttest.CaptureOutput(t, func() int { return Launch("nosuchalias", []string{"--help"}) })
	if code != 0 || !strings.Contains(out, "FAKE-CLAUDE-HELP") {
		t.Fatalf("Launch(--help) = %d, stdout %q, stderr %q; want 0 and claude's help", code, out, errOut)
	}
}

// Only a lone help flag takes the shortcut; a subcommand's --help goes through to claude.
func TestBareHelpKeepsSubcommandHelp(t *testing.T) {
	for args, want := range map[string]bool{"--help": true, "-h": true, "mcp --help": false, "-p hi --help": false} {
		if got := bareHelp(strings.Fields(args)); got != want {
			t.Errorf("bareHelp(%q) = %v, want %v", args, got, want)
		}
	}
}
