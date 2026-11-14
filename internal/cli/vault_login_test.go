package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const loginSecret = "DATABRICKS_HAND_OAUTH"

// loginTokenServer fakes the workspace token endpoint: ok mints a one-hour JWT, otherwise it refuses with invalid_grant.
func loginTokenServer(t *testing.T, ok bool) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant", "error_description": "expired"})
			return
		}
		jwt := hosttest.UnsignedJWT(fmt.Sprintf(`{"iss":"x","exp":%d}`, time.Now().Add(time.Hour).Unix()))
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": jwt, "expires_in": 3600})
	}))
	t.Cleanup(srv.Close)
	vaulttest.UseFreshKey(t)
	vaulttest.Put(t, vault.Overlay, "providers", fmt.Sprintf(
		`{"providers":{"databricks":{"hand":{"protocol":"anthropic","base_url":%q,"auth":{"type":"oauth"}}}}}`, srv.URL+"/serving-endpoints/anthropic"))
	return &hits
}

func TestVaultSetLoginVerifiesAndSeals(t *testing.T) {
	hits := loginTokenServer(t, true)
	withStdin(t, `{"refresh":"hand-refresh-token"}`+"\n")
	code, _, stderr := vaultRun(t, "set", loginSecret)
	if code != 0 || hits.Load() != 1 || !strings.Contains(stderr, "hand: stored login verified and sealed · token expires in ") {
		t.Fatalf("exit %d, token hits %d, stderr %q; want one refresh and the sealed line", code, hits.Load(), stderr)
	}
	if got, _, _ := vault.ReadSecret(loginSecret); !strings.Contains(got, `"sealed"`) {
		t.Error("the stored record carries no seal time")
	}
}

func TestVaultSetLoginRefusedStaysStoredAndNamesTheCommand(t *testing.T) {
	loginTokenServer(t, false)
	withStdin(t, `{"refresh":"hand-refresh-token"}`+"\n")
	code, stdout, stderr := vaultRun(t, "set", loginSecret)
	if code != 0 || !strings.Contains(stderr, "rejected it") || !strings.Contains(stderr, "`"+providers.LoginCommand("hand")+"`") {
		t.Fatalf("exit %d, stderr %q; want the rejection line naming the sign-in command", code, stderr)
	}
	if strings.Contains(stdout+stderr, "hand-refresh-token") {
		t.Error("output carries the token")
	}
	if got, found, _ := vault.ReadSecret(loginSecret); !found || !strings.Contains(got, "hand-refresh-token") {
		t.Error("a refused login must stay stored")
	}
}

func TestVaultSetLoginBareTokenIsRefusedBeforeAnyWrite(t *testing.T) {
	hits := loginTokenServer(t, true)
	withStdin(t, "bare-refresh-token\n")
	code, stdout, stderr := vaultRun(t, "set", loginSecret)
	if code == 0 || !strings.Contains(stderr, `{"refresh":"<refresh token>"}`) || strings.Contains(stdout+stderr, "bare-refresh-token") {
		t.Errorf("exit %d, stderr %q; want a refusal naming the record shape, without the value", code, stderr)
	}
	if _, found, _ := vault.ReadSecret(loginSecret); found || hits.Load() != 0 {
		t.Errorf("written %v, token hits %d; a refused value is neither stored nor tried", found, hits.Load())
	}
}

func TestVaultSetLoginDryRunReportsWithoutANetworkCall(t *testing.T) {
	hits := loginTokenServer(t, true)
	withStdin(t, `{"refresh":"hand-refresh-token"}`+"\n")
	code, stdout, _ := vaultRun(t, "set", loginSecret, "--dry-run")
	if code != 0 || !strings.Contains(stdout, "would verify and seal") || hits.Load() != 0 {
		t.Errorf("exit %d, stdout %q, token hits %d; want the note and no call", code, stdout, hits.Load())
	}
	if _, found, _ := vault.ReadSecret(loginSecret); found {
		t.Error("dry run stored the login")
	}
}

func TestVaultSetOtherSecretMakesNoTokenCall(t *testing.T) {
	hits := loginTokenServer(t, true)
	withStdin(t, "plain-value\n")
	code, _, stderr := vaultRun(t, "set", "SOME_OTHER_SECRET")
	if code != 0 || hits.Load() != 0 || strings.Contains(stderr, "verified") {
		t.Errorf("exit %d, token hits %d, stderr %q; an ordinary secret is never verified", code, hits.Load(), stderr)
	}
}

func TestVaultImportVerifiesLoginsAndSkipsBareTokens(t *testing.T) {
	hits := loginTokenServer(t, true)
	file := filepath.Join(t.TempDir(), "in.env")
	body := loginSecret + `='{"refresh":"hand-refresh-token"}'` + "\nDATABRICKS_BARE_OAUTH=bare-token\nPLAIN=value\n"
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := vaultRun(t, "import", file, "--yes")
	if code != 0 || hits.Load() != 1 || !strings.Contains(stderr, "hand: stored login verified and sealed") {
		t.Errorf("exit %d, token hits %d, stderr %q; want one verified login", code, hits.Load(), stderr)
	}
	if _, found, _ := vault.ReadSecret("PLAIN"); !found {
		t.Error("the ordinary pair was not imported")
	}
}
