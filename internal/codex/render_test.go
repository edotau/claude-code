package codex

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentrender"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// scratch points CLAUDE_CONFIG_DIR at a temp dir with a keyed default provider and returns the vendor-config home.
func scratch(t *testing.T) string {
	t.Helper()
	configDir := vaulttest.Use(t)
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HARNESS_PROVIDER", "")
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	t.Cleanup(agent.StubRouter("http://127.0.0.1:9999"))
	vaulttest.Put(t, vault.Overlay, "providers", `{"default": "openai"}`)
	if err := os.MkdirAll(filepath.Join(configDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestCodexCredStoreKeyIgnoresMultilineInstructions(t *testing.T) {
	for _, quote := range []string{`"""`, `'''`} {
		t.Run(quote, func(t *testing.T) {
			existing := "instructions = " + quote + "\n  cli_auth_credentials_store = \"file\"\n" + quote + "\n"
			want := "cli_auth_credentials_store = \"keyring\"\n"
			if got := codexCredStoreKey(existing, io.Discard); got != want {
				t.Fatalf("multiline instructions suppressed the keyring setting: got %q, want %q", got, want)
			}
		})
	}
}

func TestSplitTopLevelInlineKeepsQuotedDelimiters(t *testing.T) {
	in := `{ name = "x", http_headers = { "anthropic-beta" = "a,b}{" }, lit = 'c,d' }`
	want := []string{
		`name = "x"`,
		`http_headers = { "anthropic-beta" = "a,b}{" }`,
		`lit = 'c,d'`,
	}
	if got := splitTopLevelInline(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSplitTopLevelInlineEscapedQuote(t *testing.T) {
	in := `{ a = "x\",y", b = 1 }`
	want := []string{`a = "x\",y"`, `b = 1`}
	if got := splitTopLevelInline(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Approval/sandbox top-level keys land before the first [table] header and a second render is byte-identical.
func TestCodexApprovalKeysPrecedeExistingTables(t *testing.T) {
	home := scratch(t)
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := "[projects.\"/x\"]\ntrust_level = \"trusted\"\n\n[hooks.state.\"k\"]\ntrusted_hash = \"sha256:deadbeef\"\n"
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := agentrender.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	assertCodexApprovalBeforeForeignTables(t, content)

	var out2 bytes.Buffer
	if _, err := agentrender.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out2, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("second agentrender.RenderConfigs: %v\n%s", err, out2.String())
	}
	raw2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw2) != content {
		t.Errorf("second run not byte-identical:\nfirst:\n%s\nsecond:\n%s", content, raw2)
	}
}

func assertCodexApprovalBeforeForeignTables(t *testing.T, content string) {
	t.Helper()
	firstTable := strings.Index(content, "[")
	if firstTable < 0 {
		t.Fatalf("no table header found at all:\n%s", content)
	}
	approvalIdx := strings.Index(content, "approval_policy")
	sandboxIdx := strings.Index(content, "sandbox_mode")
	if approvalIdx < 0 || sandboxIdx < 0 {
		t.Fatalf("approval_policy/sandbox_mode not rendered:\n%s", content)
	}
	// Before the first "[" means top-level: TOML has no way for a key there to belong to any table.
	if approvalIdx > firstTable || sandboxIdx > firstTable {
		t.Errorf("approval_policy/sandbox_mode (at %d, %d) must precede the first [table] header (at %d):\n%s",
			approvalIdx, sandboxIdx, firstTable, content)
	}
	if !strings.Contains(content, "[projects.\"/x\"]") || !strings.Contains(content, "trust_level = \"trusted\"") {
		t.Errorf("foreign [projects] table was dropped or altered:\n%s", content)
	}
	if !strings.Contains(content, "[hooks.state.\"k\"]") || !strings.Contains(content, "trusted_hash = \"sha256:deadbeef\"") {
		t.Errorf("foreign [hooks.state] table was dropped or altered:\n%s", content)
	}
}

// topLevelKeyCount counts "key = " assignments before the first top-level [table] header.
func topLevelKeyCount(content, key string) int {
	count := 0
	for _, line := range strings.SplitAfter(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			break
		}
		if strings.HasPrefix(trimmed, key+" ") {
			count++
		}
	}
	return count
}

// The banner replaces a top-level approval_policy (no duplicate) and leaves one inside [profiles.x] alone.
func TestCodexBannerReplacesExistingTopLevelPermissionKeys(t *testing.T) {
	home := scratch(t)
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := "approval_policy = \"on-request\"\n\n[profiles.x]\napproval_policy = \"untrusted\"\n"
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := agentrender.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)

	if n := topLevelKeyCount(content, "approval_policy"); n != 1 {
		t.Fatalf("top-level approval_policy count = %d, want 1 (stale key must be removed, not duplicated):\n%s", n, content)
	}
	if !strings.Contains(content, "approval_policy = \"never\"") {
		t.Errorf("top-level approval_policy must be the banner's value (never):\n%s", content)
	}
	if !strings.Contains(content, "[profiles.x]") || !strings.Contains(content, "approval_policy = \"untrusted\"") {
		t.Errorf("profiles.x table's approval_policy must stay intact:\n%s", content)
	}
}
