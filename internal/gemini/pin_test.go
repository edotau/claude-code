package gemini_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// pinFixture is a hermetic registry with two Databricks gateway rows (dev, gw), a fake agy Google login and the session on session.
func pinFixture(t *testing.T, session string) {
	t.Helper()
	vaulttest.Use(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CLAUDE_PROFILE", session)
	t.Cleanup(agent.StubRouter("http://127.0.0.1:9999"))
	row := func(host string) string {
		return `{"preset":"databricks","host":"https://` + host + `.example.com","auth":{"type":"pat"},"models":{"opus":"databricks-claude-opus"}}`
	}
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":`+row("dev")+`,"gw":`+row("gw")+`}}}`)
	marker := filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token")
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func resolveGemini(t *testing.T, req agent.RunOptions) (providers.Target, bool, error) {
	t.Helper()
	agent.NoticeOut = &strings.Builder{}
	return agent.SchemaFor("gemini").TargetCtx(context.Background(), req)
}

// The gemini vault record beats the Databricks session profile: profile picks the row, model the id, flags beat both.
func TestGeminiRecordBeatsSessionProfile(t *testing.T) {
	for _, c := range []struct {
		name, session, profile, model string
		req                           agent.RunOptions
		wantProv, wantModel, wantErr  string
	}{
		{"A model pin on the Google login", "dev", "", "gemini-pinned", agent.RunOptions{}, "antigravity", "gemini-pinned", ""},
		{"B pinned gateway row beats the session row", "dev", "gw", "gemini-y", agent.RunOptions{}, "gw", "gemini-y", ""},
		{"C pin names another row than the gateway session", "gw", "dev", "", agent.RunOptions{}, "dev", "", ""},
		{"C gateway session without a pin stays on the Google login", "gw", "", "", agent.RunOptions{}, "antigravity", "", ""},
		{"D --model beats the record model", "dev", "gw", "gemini-y", agent.RunOptions{Model: "gemini-flag"}, "gw", "gemini-flag", ""},
		{"D --provider beats the record profile", "dev", "gw", "", agent.RunOptions{Provider: "dev"}, "dev", "", ""},
		{"E own-login label is a valid pin", "dev", "antigravity", "", agent.RunOptions{}, "antigravity", "", ""},
		{"E pin naming no registry row is refused", "dev", "nosuch", "", agent.RunOptions{}, "", "", `pinned profile: gemini: unknown profile "nosuch"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			pinFixture(t, c.session)
			vaulttest.SetRecord(t, "gemini", "profile", c.profile)
			vaulttest.SetRecord(t, "gemini", "model", c.model)
			tg, _, err := resolveGemini(t, c.req)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, c.wantErr)
				}
				return
			}
			if err != nil || tg.Provider == nil || tg.Provider.Name != c.wantProv {
				t.Fatalf("target = %+v err = %v, want provider %s", tg, err, c.wantProv)
			}
			if c.wantModel != "" && tg.Model != c.wantModel {
				t.Errorf("model = %q, want %q", tg.Model, c.wantModel)
			}
		})
	}
}

// A gateway session row that no record names is refused when agy has no Google login to fall back on.
func TestGeminiGatewaySessionWithoutPinOrLoginIsRefused(t *testing.T) {
	pinFixture(t, "gw")
	if err := os.RemoveAll(filepath.Join(os.Getenv("HOME"), ".gemini")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveGemini(t, agent.RunOptions{}); err == nil || !strings.Contains(err.Error(), "Google login") {
		t.Fatalf("err = %v, want the Google-login refusal", err)
	}
}
