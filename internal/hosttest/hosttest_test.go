package hosttest

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(Main(m)) }

func TestHostIsHermetic(t *testing.T) {
	for _, vendor := range []string{"claude", "codex", "copilot", "opencode", "agy", "gemini", "tmux", "tailscale", "launchctl"} {
		if p, err := exec.LookPath(vendor); err == nil {
			t.Errorf("%s visible at %s", vendor, p)
		}
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Errorf("sh missing from the allowlisted PATH: %v", err)
	}
	cfg, home := os.Getenv("CLAUDE_CONFIG_DIR"), os.Getenv("HOME")
	if !strings.HasPrefix(cfg, home) || !strings.Contains(home, "hosttest-") {
		t.Errorf("CLAUDE_CONFIG_DIR %q / HOME %q not in the temp dir", cfg, home)
	}
	if os.Getenv("HARNESS_VAULT_KEY") == "" || os.Getenv("WSL_DISTRO_NAME") != "" {
		t.Error("vault key unset or WSL marker leaked")
	}
}
