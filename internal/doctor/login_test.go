package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// loginRig runs the credential check of one stored oauth row (the session profile) with a verified service-principal
// probe; record is "dead", "healthy" (an expiry record) or "" (nothing recorded).
func loginRig(t *testing.T, record string, seals []audit.Event) check {
	t.Helper()
	vaulttest.Use(t)
	p := &providers.Provider{Name: "dbx", Provider: "databricks",
		Auth: providers.Auth{Type: providers.AuthOAuth, SPClientID: "sp-client"}}
	if err := vault.Store(providers.AuthSecretName(p), `{"refresh":"x"}`); err != nil {
		t.Fatal(err)
	}
	if err := vault.Store(providers.SecretName("databricks", "dbx", providers.AuthM2M), "secret"); err != nil {
		t.Fatal(err)
	}
	plantLoginRecord(t, record)
	oldProbe, oldAudit := probeCredential, readAudit
	t.Cleanup(func() { probeCredential, readAudit = oldProbe, oldAudit })
	probeCredential = func(context.Context, *providers.Provider) (string, string) { return providers.ProbeVerified, "" }
	readAudit = func(time.Time) ([]audit.Event, error) { return seals, nil }
	reg := &providers.Registry{Default: "dbx", Providers: map[string]*providers.Provider{"dbx": p}}
	sel := providers.Selection{Provider: p}
	return providerCredentialChecks(context.Background(), reg, sel, nil, []string{"dbx"})[0]
}

// plantLoginRecord writes the dead-login mark or the expiry record doctor reads; the rig has no refresh endpoint.
func plantLoginRecord(t *testing.T, record string) {
	t.Helper()
	dir, body := "", ""
	switch record {
	case "dead":
		dir, body = "login-state", `{"schema":1,"state":"needs_login","since":"2026-11-14T09:00:00Z","until":"2026-11-14T09:10:00Z"}`
	case "healthy":
		dir, body = "token-expiry", fmt.Sprintf(`{"schema":2,"exp":%d,"method":"u2m"}`, time.Now().Add(time.Hour).Unix())
	default:
		return
	}
	d := filepath.Join(paths.StateDir(), "cache", dir)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "dbx.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A dead user login reads from the cache as a soft warning (never a hard failure); healthy adds nothing, no record
// says unknown, and the diagnostic leaves the cache untouched.
func TestCredentialRowReportsUserLoginFromRecords(t *testing.T) {
	assertDeadLoginRow(t, loginRig(t, "dead", nil))
	if healthy := loginRig(t, "healthy", nil); !healthy.OK || !healthy.Hard || strings.Contains(healthy.Detail, "user login") {
		t.Errorf("healthy row = %+v, want ok, hard, and no login text", healthy)
	}
	unknown := loginRig(t, "", nil)
	if !unknown.OK || !strings.Contains(unknown.Detail, "user login unknown") {
		t.Errorf("unrecorded row = %+v, want ok with user login unknown", unknown)
	}
	if _, err := os.Stat(filepath.Join(paths.StateDir(), "cache", "login-state")); !os.IsNotExist(err) {
		t.Errorf("doctor wrote a login-state record for an unrecorded login: %v", err)
	}
}

// assertDeadLoginRow checks the dead-login warning row: wording, soft marks and an unchanged cache record.
func assertDeadLoginRow(t *testing.T, dead check) {
	t.Helper()
	for _, want := range []string{"verified", "user login dead (needs_login), using the service principal: " +
		"run claude-code databricks login --profile dbx"} {
		if !strings.Contains(dead.Detail, want) {
			t.Errorf("dead login row %q lacks %q", dead.Detail, want)
		}
	}
	if dead.OK || dead.Hard {
		t.Errorf("dead login row ok=%v hard=%v, want a soft warning (ok=false hard=false)", dead.OK, dead.Hard)
	}
	raw, err := os.ReadFile(filepath.Join(paths.StateDir(), "cache", "login-state", "dbx.json"))
	if err != nil || !strings.Contains(string(raw), `"2026-11-14T09:10:00Z"`) {
		t.Errorf("doctor changed the dead-login record: %q (%v)", raw, err)
	}
}

// The newest seal failure of the profile's own secrets shows its age; another profile's failure does not.
func TestCredentialRowReportsARecentSealFailure(t *testing.T) {
	seals := []audit.Event{
		{Time: time.Now().Add(-5 * time.Hour), Action: audit.SealFailed, Resource: "DATABRICKS_DBX_M2M_ACCESS", Detail: "sp-access"},
		{Time: time.Now().Add(-3 * time.Hour), Action: audit.SealFailed, Resource: "DATABRICKS_DBX_OAUTH", Detail: "refresh"},
		{Time: time.Now().Add(-time.Minute), Action: audit.SealFailed, Resource: "DATABRICKS_OTHER_OAUTH", Detail: "refresh"},
		{Time: time.Now().Add(-time.Minute), Action: audit.SecretSet, Resource: "DATABRICKS_DBX_OAUTH"},
	}
	if got := loginRig(t, "healthy", seals).Detail; !strings.Contains(got, "last token seal failed 3h ago (audit: seal_failed)") {
		t.Errorf("row %q lacks the 3h seal failure", got)
	}
	if got := loginRig(t, "healthy", seals[2:3]).Detail; strings.Contains(got, "seal failed") {
		t.Errorf("row %q reports another profile's seal failure", got)
	}
}
