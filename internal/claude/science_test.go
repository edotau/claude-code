package claude

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Science unsets the harness's own session/routing env (shell and vault custom env), keeps claude-science's own
// CLAUDE_SCIENCE_* and ANTHROPIC_* knobs, and passes argv through untouched.
func TestScienceLaunchPlanStripsHarnessEnvOnly(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Env, "HARNESS_WORKTREE", "off")
	vaulttest.Put(t, vault.Env, "MY_OWN_VAR", "1")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude-science"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	env := []string{"ANTHROPIC_BASE_URL=http://127.0.0.1:4000/p/x", "ANTHROPIC_CUSTOM_HEADERS=x-harness: s",
		"CLAUDE_CODE_SSE_PORT=1", "CLAUDECODE=1", "HARNESS_PROVIDER=p", "GEMINI_CLI_IDE_AUTH_TOKEN=t",
		"CLAUDE_SCIENCE_TOKEN=keep", "ANTHROPIC_ORGANIZATION_ID=keep", "HOME=/h"}
	p, err := Science(env, []string{"serve", "--model", "x"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS", "CLAUDE_CODE_SSE_PORT", "CLAUDECODE",
		"HARNESS_PROVIDER", "GEMINI_CLI_IDE_AUTH_TOKEN", "HARNESS_WORKTREE"}
	if !slices.Equal(p.Unset, want) || !slices.Equal(p.Argv, []string{"serve", "--model", "x"}) {
		t.Errorf("unset %q argv %q, want unset %q and argv passed through", p.Unset, p.Argv, want)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := Science(env, nil); err == nil {
		t.Error("a missing claude-science must error, not exec")
	}
}

func TestScienceConfigPortTopLevelOnly(t *testing.T) {
	cases := map[string]int{
		"port = 4100\n":                  4100,
		"port = 4100 # app\n[sandbox]\n": 4100,
		"[sandbox]\nport = 4100\n":       3000,
		"":                               3000,
		"port = nope\n":                  3000,
	}
	for in, want := range cases {
		if got := scienceConfigPort([]byte(in)); got != want {
			t.Errorf("scienceConfigPort(%q) = %d, want %d", in, got, want)
		}
	}
	if got := scienceConfigPort(scienceTOML); got != 3000 {
		t.Errorf("embedded science.toml port = %d, want 3000", got)
	}
}

func TestScienceArgsLeadingPortOnly(t *testing.T) {
	port, rest, err := scienceArgs([]string{"--port=4100", "serve", "--port", "9"}, 3000)
	if err != nil || port != 4100 || !slices.Equal(rest, []string{"serve", "--port", "9"}) {
		t.Fatalf("got %d %v %v", port, rest, err)
	}
	if _, _, err := scienceArgs([]string{"--port", "x"}, 3000); err == nil {
		t.Fatal("bad --port must error")
	}
}

func TestScienceGlobalOptionsKeepPortPin(t *testing.T) {
	port, rest, err := scienceArgs([]string{"--port", "4100", "--config", "/custom.toml", "serve"}, 3000)
	if err != nil {
		t.Fatal(err)
	}
	var checked []int
	got, err := scienceArgv(rest, "/default.toml", port, func(p int) bool {
		checked = append(checked, p)
		return false
	})
	want := []string{"--config", "/custom.toml", "serve", "--port", "4100", "--sandbox-port", "4101"}
	if err != nil || !slices.Equal(got, want) || !slices.Equal(checked, []int{4100, 4101}) {
		t.Fatalf("argv = %v, checked = %v, err = %v; want %v, ports 4100/4101, nil", got, checked, err, want)
	}
	for _, busyPort := range []int{4100, 4101} {
		if _, err := scienceArgv(rest, "", port, func(p int) bool { return p == busyPort }); err == nil {
			t.Errorf("busy pinned port %d was ignored", busyPort)
		}
	}
}

func TestScienceArgvPinsServeAndConfig(t *testing.T) {
	free := func(int) bool { return false }
	got, err := scienceArgv([]string{"serve"}, "/c.toml", 3000, free)
	want := []string{"serve", "--config", "/c.toml", "--port", "3000", "--sandbox-port", "3001"}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("serve argv = %v, %v; want %v", got, err, want)
	}
	if got, _ := scienceArgv([]string{"status", "--config", "x"}, "/c.toml", 3000, free); !slices.Equal(got, []string{"status", "--config", "x"}) {
		t.Errorf("explicit --config must win: %v", got)
	}
	if got, _ := scienceArgv([]string{"serve", "--port", "5"}, "", 3000, free); !slices.Equal(got, []string{"serve", "--port", "5"}) {
		t.Errorf("vendor's own serve --port must pass untouched: %v", got)
	}
	busy := func(p int) bool { return p == 3001 }
	if _, err := scienceArgv([]string{"serve"}, "", 3000, busy); err == nil {
		t.Error("a busy sandbox port must error, not drift")
	}
}
