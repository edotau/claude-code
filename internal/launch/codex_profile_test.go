package launch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func launchCodexProfile(t *testing.T, profile string, withFile bool) error {
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
	opts, rest, err := ParseArgs([]string{"--profile", profile}, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Vendor(context.Background(), "codex", opts, rest)
	return err
}

func TestCodexProfileMatchesConfigFileHintsDashP(t *testing.T) {
	err := launchCodexProfile(t, "work", true)
	if err == nil || !strings.Contains(err.Error(), "use -p work") {
		t.Fatalf("err = %v, want message containing %q", err, "use -p work")
	}
}

func TestCodexProfileUnknownKeepsOldError(t *testing.T) {
	err := launchCodexProfile(t, "nosuch", false)
	if err == nil || !strings.Contains(err.Error(), `unknown provider "nosuch"`) || strings.Contains(err.Error(), "use -p") {
		t.Fatalf("err = %v, want the unknown-provider error without a -p hint", err)
	}
}
