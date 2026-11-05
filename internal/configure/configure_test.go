package configure

import (
	"context"
	"testing"

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
