package vendors_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/launch"
)

func std2CodexLaunch(t *testing.T, profile string, withFile bool) error {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("PATH", t.TempDir())
	if withFile {
		if err := os.WriteFile(filepath.Join(codexHome, profile+".config.toml"), []byte("model = \"x\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	opts, rest, err := launch.ParseArgs([]string{"--profile", profile}, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = launch.Vendor(context.Background(), "codex", opts, rest)
	return err
}

func TestStd2CodexProfileMatchesConfigFileHintsDashP(t *testing.T) {
	err := std2CodexLaunch(t, "work", true)
	if err == nil || !strings.Contains(err.Error(), "use -p work") {
		t.Fatalf("err = %v, want message containing %q", err, "use -p work")
	}
}

func TestStd2CodexProfileUnknownKeepsOldError(t *testing.T) {
	err := std2CodexLaunch(t, "nosuch", false)
	if err == nil || !strings.Contains(err.Error(), `unknown provider "nosuch"`) || strings.Contains(err.Error(), "use -p") {
		t.Fatalf("err = %v, want the unknown-provider error without a -p hint", err)
	}
}
