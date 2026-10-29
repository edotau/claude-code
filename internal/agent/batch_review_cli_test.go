package agent_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// The vault's custom env holds every profile's vars; a batch child on pa gets pa's and never pb's credential var.
func TestBatchChildDropsOtherProfilesCustomCredentialVar(t *testing.T) {
	cliEnv(t)
	vaulttest.Use(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("PA_BATCH_KEY", "TOKEN-AAAA-1111")
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"anthropic":{`+
		`"pa":{"protocol":"anthropic","base_url":"http://127.0.0.1:11111","auth":{"type":"key","env":"PA_BATCH_KEY"},"models":{"opus":"m-a","sonnet":"m-a","haiku":"m-a"}},`+
		`"pb":{"protocol":"anthropic","base_url":"http://127.0.0.1:22222","auth":{"type":"key","env":"PB_BATCH_KEY"},"models":{"opus":"m-b","sonnet":"m-b","haiku":"m-b"}}}}}`)
	if err := providers.SetCustomEnv("PB_BATCH_KEY", "PB-CUSTOM-SECRET"); err != nil {
		t.Fatal(err)
	}
	if err := providers.SetCustomEnv("PA_CUSTOM_NOTE", "pa-note"); err != nil {
		t.Fatal(err)
	}
	dump := filepath.Join(t.TempDir(), "child.env")
	pathstest.FakeVendor(t, "claude", "cat > /dev/null\nenv > "+dump+"\n"+`printf '%s\n' '{"type":"result","result":"hello"}'`)
	file := batchFile(t, taskLine(t, map[string]any{"id": "a", "agent": "claude", "profile": "pa", "prompt": "x", "workdir": t.TempDir(), "tools": true}))
	if code, out, stderr := runCLI(t, "ask", "--batch", file); code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, stderr)
	}
	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	env := string(raw)
	if strings.Contains(env, "PB-CUSTOM-SECRET") {
		t.Error("pa's child holds pb's custom credential var")
	}
	if !strings.Contains(env, "TOKEN-AAAA-1111") || !strings.Contains(env, "pa-note") {
		t.Errorf("pa's child lost its own token or an unrelated custom var:\n%s", env)
	}
}
