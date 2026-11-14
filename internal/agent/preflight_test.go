package agent

import (
	"context"
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

// TestCheckModel1MAliasPassesLikeBareAlias: codex on a Databricks profile treats <alias>[1m] exactly as the bare alias.
func TestCheckModel1MAliasPassesLikeBareAlias(t *testing.T) {
	vaulttest.Use(t)
	p := &providers.Provider{Name: "dev", Preset: providers.PresetDatabricks, Host: "https://dev.example.com"}
	now := time.Now()
	for _, alias := range []string{"opus", "sonnet", "haiku"} {
		bareWire, bareFailed, bareNote := checkModel("codex", p, alias, now)
		wire, failed, note := checkModel("codex", p, alias+"[1m]", now)
		if failed != nil || bareFailed != nil || note != bareNote || wire != alias+"[1m]" || bareWire != alias {
			t.Errorf("%s[1m]: wire %q failed %v note %q; bare %s: wire %q failed %v note %q",
				alias, wire, failed, note, alias, bareWire, bareFailed, bareNote)
		}
	}
}

// A preview leads only with an explicit lead; the session profile alone adds none (callers behave as before).
func TestPreviewRouteLeadsOnlyWhenAsked(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.SetRecord(t, "codex", "profile", "chatgpt")
	t.Setenv("CLAUDE_PROFILE", "no-such-row")
	for _, lead := range []string{"", "no-such-row"} {
		q := RouteReq{Agent: "codex", Task: "x", Lead: lead, Fallback: router.FallbackAll, Preview: true}
		_, res, err := Resolve(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		var skips []string
		for _, s := range res.Skipped {
			skips = append(skips, s.Reason)
		}
		if got := strings.Contains(strings.Join(skips, "\n"), "no-such-row"); got != (lead != "") {
			t.Fatalf("lead %q: skip naming no-such-row = %v, skipped %+v", lead, got, res.Skipped)
		}
	}
}

// A candidate dropped after the pick must not lend its slot to the next survivor of the same agent and account.
func TestPreviewOutcomeMatchesByRungNotPosition(t *testing.T) {
	cell := func(m string) router.Cell { return router.Cell{Model: m} }
	pick := router.PickResult{Candidates: []router.Candidate{
		{Agent: "codex", Cell: cell("m1"), Profile: "A"},
		{Agent: "codex", Cell: cell("m2"), Profile: "B"},
	}}
	chain := Chain{Dispatches: []Dispatch{{Agent: &pinFallbackRunner{name: "codex"}, Model: "m2", rung: 2}}}
	got := previewOutcome(chain, pick).Candidates
	if len(got) != 1 || got[0].Profile != "B" || got[0].Model != "m2" {
		t.Fatalf("candidates = %+v, want only rung 2 (profile B, model m2)", got)
	}
}

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

// A dead user login fails the local check when no service principal covers it, and names the command; an SP or a
// healthy login passes.
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
