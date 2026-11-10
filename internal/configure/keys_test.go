package configure

import (
	"context"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
)

func TestRunMintUsesInstalledVendor(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	const token = "sk-ant-oat01-abcdefghijklmnopqrstuv"
	pathstest.FakeVendor(t, "claude", "printf '%s\\n' '"+token+"'")
	got, err := RunMintContext(context.Background(), []string{"claude", "setup-token"})
	if err != nil || got != token {
		t.Fatalf("mint = %q, %v; want installed vendor's token", got, err)
	}
}
func TestVendorArgv(t *testing.T) {
	for _, name := range []string{"claude", "codex", "copilot", "opencode", "gemini", "agy"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			t.Setenv("PATH", t.TempDir())
			bin := pathstest.FakeVendor(t, name, "exit 0")
			argv := []string{name, "login", "argument with spaces"}
			original := slices.Clone(argv)
			got, err := cliArgv(argv)
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{bin, "login", "argument with spaces"}; !slices.Equal(got, want) {
				t.Errorf("cliArgv = %v, want %v", got, want)
			}
			if !slices.Equal(argv, original) {
				t.Errorf("input changed to %v, want %v", argv, original)
			}
			got[1] = "changed"
			if !slices.Equal(argv, original) {
				t.Errorf("result aliases input: %v", argv)
			}
		})
	}
}

func TestVendorArgvGithubUnchanged(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	argv := []string{"gh", "auth", "login"}
	got, err := cliArgv(argv)
	if err != nil || !slices.Equal(got, argv) {
		t.Errorf("cliArgv = %v, %v; want %v, nil", got, err, argv)
	}
}

func TestVendorArgvMissingVendor(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	argv := []string{"copilot", "login"}
	original := slices.Clone(argv)
	_, wantErr := paths.AgentBin("copilot")
	if wantErr == nil {
		t.Fatal("expected missing vendor")
	}
	got, err := cliArgv(argv)
	if got != nil || err == nil || err.Error() != wantErr.Error() {
		t.Errorf("cliArgv = %v, %v; want nil, %v", got, err, wantErr)
	}
	if !slices.Equal(argv, original) {
		t.Errorf("input changed to %v, want %v", argv, original)
	}
}

// A failed env write after the secret is sealed must say the secret was already saved.
