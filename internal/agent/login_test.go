package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// plantDeadLogin writes the dead-login mark providers.LoginStatus reads for the profile.
func plantDeadLogin(t *testing.T, name string) {
	t.Helper()
	dir := filepath.Join(paths.StateDir(), "cache", "login-state")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	rec := fmt.Sprintf(`{"schema":1,"state":"needs_login","since":%q,"until":%q}`, now, now)
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A dead user login fails the local check when no service principal covers it, and names the command; an SP or a
// healthy login passes.
// dbxRow stores an OAuth databricks provider with a refresh token in the vault.
func dbxRow(t *testing.T, name, sp string) *providers.Provider {
	t.Helper()
	p := &providers.Provider{Name: name, Provider: "databricks", Preset: providers.PresetDatabricks,
		Auth: providers.Auth{Type: providers.AuthOAuth, SPClientID: sp}}
	if err := vault.Store(providers.AuthSecretName(p), `{"refresh":"x"}`); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckWorkspaceFailsADeadLoginOnlyWithoutAServicePrincipal(t *testing.T) {
	vaulttest.Use(t)
	alone, paired, live := dbxRow(t, "dbx-alone", ""), dbxRow(t, "dbx-paired", "sp-client"), dbxRow(t, "dbx-live", "")
	if err := vault.Store(providers.SecretName("databricks", "dbx-paired", providers.AuthM2M), "secret"); err != nil {
		t.Fatal(err)
	}
	plantDeadLogin(t, "dbx-alone")
	plantDeadLogin(t, "dbx-paired")
	now := time.Now()

	fs := CheckWorkspace(alone, router.State{}, now)
	if len(fs) != 1 || fs[0].Check != "login" || fs[0].Profile != "dbx-alone" ||
		!strings.Contains(fs[0].Detail, "claude-code databricks login --profile dbx-alone") {
		t.Errorf("dead login, no SP: findings %+v, want one login finding naming the command", fs)
	}
	if fs := CheckWorkspace(paired, router.State{}, now); len(fs) != 0 {
		t.Errorf("dead login with a paired SP: findings %+v, want none", fs)
	}
	paired.Identity = providers.IdentityUser
	if fs := CheckWorkspace(paired, router.State{}, now); len(fs) != 1 || fs[0].Check != "login" {
		t.Errorf("dead login, identity user: findings %+v, want the login finding (the SP is not used)", fs)
	}
	if fs := CheckWorkspace(live, router.State{}, now); len(fs) != 0 {
		t.Errorf("healthy login: findings %+v, want none", fs)
	}
}
