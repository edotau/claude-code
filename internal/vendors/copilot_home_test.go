package vendors

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The shared private home: one COPILOT_HOME for launch and SDK, the login guard fires, dry touches nothing.
func TestPrepareCopilotHome(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("COPILOT_HOME", "")
	t.Setenv("HOME", t.TempDir())
	home := filepath.Join(cfg, "state", "copilot", "registry-home")

	if _, err := PrepareCopilotHome(true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home); err == nil {
		t.Fatal("dry run created the home")
	}
	ph, err := PrepareCopilotHome(false)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(home); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("home = %v, %v; want 0700 dir", fi, err)
	}
	for _, want := range []string{"COPILOT_HOME=" + home, "DBUS_SESSION_BUS_ADDRESS="} {
		if !slices.Contains(ph.Env, want) {
			t.Errorf("env %v lacks %q", ph.Env, want)
		}
	}
	for _, kv := range ph.Env {
		if strings.HasPrefix(kv, "HOME=") {
			t.Errorf("env overrides HOME: %q", kv)
		}
	}
	if !slices.Contains(ph.Unset, "GITHUB_ENTERPRISE_TOKEN") || !slices.Contains(ph.Unset, "COPILOT_SDK_AUTH_TOKEN") {
		t.Errorf("unset %v misses the union list", ph.Unset)
	}
	login := `{"loggedInUsers":[{"login":"x"}]}`
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(login), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareCopilotHome(false); err == nil {
		t.Fatal("login guard did not refuse a home holding a login")
	}
}
