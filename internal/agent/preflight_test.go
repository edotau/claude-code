package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
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

func TestPreviewOutcomeKeepsConcreteClaudeDispatch(t *testing.T) {
	pick := router.PickResult{Candidates: []router.Candidate{{
		Agent: "claude", Profile: "dev", Model: "databricks-claude-opus-5-5",
	}}}
	chain := Chain{Dispatches: []Dispatch{{Model: "dev:databricks-claude-opus-5", rung: 1}}}
	got := previewOutcome(chain, pick).Candidates
	if len(got) != 1 || got[0].Model != "dev:databricks-claude-opus-5" {
		t.Fatalf("preview = %+v, want explicit dev:databricks-claude-opus-5", got)
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

// A dead user login fails the local check when no service principal covers it; an SP or a healthy login passes.
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
		!strings.Contains(fs[0].Detail, "`"+providers.LoginCommand("dbx-alone")+"`") {
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

// A refusal names served models of the same vendor beyond Claude, GPT and Gemini (kimi-k3-1-pro -> kimi-k3).
func TestServedAlternativesOtherVendor(t *testing.T) {
	setConfigDir(t)
	saveListing(t, "dev", []string{"databricks-kimi-k3", "databricks-glm-5-3", "databricks-claude-opus-5-5"})
	p := &providers.Provider{Name: "dev", Preset: providers.PresetDatabricks}
	if got := servedAlternatives(p, "databricks-kimi-k3-1-pro", time.Now()); !slices.Equal(got, []string{"databricks-kimi-k3"}) {
		t.Errorf("servedAlternatives = %v; want [databricks-kimi-k3]", got)
	}
}

type preflightTransport func(*http.Request) (*http.Response, error)

func (f preflightTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPreflightAuthCacheReprobesFailuresAndRotatedCredentials(t *testing.T) {
	vaulttest.Use(t)
	p := &providers.Provider{Name: "cache", Provider: "databricks", Preset: providers.PresetDatabricks,
		Host: "https://cache.example.com", Auth: providers.Auth{Type: providers.AuthPAT}}
	name := providers.AuthSecretName(p)
	if err := vault.Store(name, "fixture"); err != nil {
		t.Fatal(err)
	}
	calls, code := 0, http.StatusOK
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = preflightTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"endpoints":[]}`))}, nil
	})
	t.Cleanup(func() { http.DefaultClient.Transport = old })
	check := func(wantFail bool) {
		t.Helper()
		f := workspaceAuth(p, router.LoadState(), time.Now())
		if (f != nil) != wantFail {
			t.Fatalf("finding = %v, want failure %v", f, wantFail)
		}
	}
	cold := time.Now()
	check(false)
	t.Logf("cold auth check: %s", time.Since(cold))
	warm := time.Now()
	check(false)
	t.Logf("warm auth check: %s", time.Since(warm))
	if calls != 1 {
		t.Fatalf("warm cache made %d probes", calls)
	}
	if err := vault.Store(name, "rotated-fixture"); err != nil {
		t.Fatal(err)
	}
	code = http.StatusUnauthorized
	check(true)
	code = http.StatusOK
	check(false)
	if calls != 3 {
		t.Fatalf("rotation and cached failure made %d probes, want 3", calls)
	}
	if err := os.WriteFile(router.StatePath(), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	check(false)
	if calls != 4 {
		t.Fatal("corrupt cache did not fall back to live")
	}
}

func TestPreflightTokenBudgetRejectsShortFixture(t *testing.T) {
	vaulttest.Use(t)
	p := &providers.Provider{Name: "token-fixture", Auth: providers.Auth{Type: providers.AuthOAuth}}
	dir := filepath.Join(paths.StateDir(), "cache", "token-expiry")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	raw := fmt.Sprintf(`{"schema":2,"exp":%d}`, now.Add(-time.Minute).Unix())
	if err := os.WriteFile(filepath.Join(dir, p.Name+".json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	f := CheckToken(p, now, 10*time.Minute)
	if f == nil || f.Check != "token" || !strings.Contains(f.Detail, providers.LoginCommand(p.Name)) {
		t.Fatalf("expired fixture finding = %v", f)
	}
	raw = fmt.Sprintf(`{"schema":2,"exp":%d}`, now.Add(14*time.Minute).Unix())
	if err := os.WriteFile(filepath.Join(dir, p.Name+".json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if f := CheckToken(p, now, 10*time.Minute); f != nil {
		t.Fatalf("sufficient lifetime: %v", f)
	}
	if f := CheckToken(p, now, DefaultToolsTimeout); f == nil {
		t.Fatal("default run accepted a short token")
	}
}

func TestCommandTokenLifetimeWarnsWithoutMaskingAuthFailures(t *testing.T) {
	vaulttest.Use(t)
	p := &providers.Provider{Name: "command-life", Provider: "databricks", Auth: providers.Auth{Type: providers.AuthOAuth}, TokenCommand: []string{"mint"}}
	vaulttest.Secret(t, providers.AuthSecretName(p), "fixture")
	dir := filepath.Join(paths.StateDir(), "cache", "token-expiry")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	raw := fmt.Sprintf(`{"schema":2,"exp":%d}`, now.Add(14*time.Minute).Unix())
	if err := os.WriteFile(filepath.Join(dir, p.Name+".json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	pf := &Preflight{}
	if fs := launchWorkspaceFindings(context.Background(), "codex", p, router.State{}, now, pf); len(fs) != 0 {
		t.Fatalf("command lifetime: findings %v, notes %v", fs, pf.Notes)
	}
	if fs := launchWorkspaceFindings(context.Background(), "claude", p, router.State{}, now, nil); len(fs) != 1 || fs[0].Check != "token" {
		t.Fatalf("static path accepted short lifetime: %v", fs)
	}
	p.TokenCommand = nil
	if fs := launchWorkspaceFindings(context.Background(), "codex", p, router.State{}, now, &Preflight{}); len(fs) != 1 || fs[0].Check != "token" {
		t.Fatalf("codex route without a credential command accepted short lifetime: %v", fs)
	}
	p.TokenCommand = []string{"mint"}
	st := router.State{router.AccountOf(p): {AuthReason: "rejected", AuthAt: now}}
	if fs := launchWorkspaceFindings(context.Background(), "codex", p, st, now, pf); len(fs) != 1 || fs[0].Check != "auth" {
		t.Fatalf("command masked auth failure: %v", fs)
	}
}

func TestLaunchWorkspaceRunTimeout(t *testing.T) {
	vaulttest.Use(t)
	p := &providers.Provider{Name: "run-timeout", Provider: "databricks", Preset: providers.PresetDatabricks,
		Host: "https://timeout.example.com", Auth: providers.Auth{Type: providers.AuthOAuth}}
	calls, old := 0, http.DefaultClient.Transport
	http.DefaultClient.Transport = preflightTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"endpoints":[]}`))}, nil
	})
	t.Cleanup(func() { http.DefaultClient.Transport = old })
	if err := vault.Store(providers.AuthSecretName(p), fmt.Sprintf(`{"access":%q,"refresh":"fixture"}`, hosttest.UnsignedJWT(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix())))); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(paths.StateDir(), "cache", "token-expiry")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, tc := range []struct {
		name          string
		timeout, life time.Duration
		wantFail      bool
	}{
		{"five minutes", 5 * time.Minute, 20 * time.Minute, false},
		{"two hours", 2 * time.Hour, time.Hour, true},
		{"default", 0, 14 * time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := fmt.Sprintf(`{"schema":2,"exp":%d}`, now.Add(tc.life).Unix())
			if err := os.WriteFile(filepath.Join(dir, p.Name+".json"), []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx := context.WithValue(context.Background(), runTimeoutKey{}, tc.timeout)
			fs := launchWorkspaceFindings(ctx, "claude", p, router.LoadState(), now, nil)
			if got := len(fs) > 0 && fs[0].Check == "token"; got != tc.wantFail {
				t.Fatalf("timeout %s, token life %s: token refusal = %v, want %v; findings %v", tc.timeout, tc.life, got, tc.wantFail, fs)
			}
		})
	}
	if calls != 1 {
		t.Fatalf("auth probes = %d, want one cached success", calls)
	}
}

// A short stored token is replaced by a headless paired-SP mint; no pairing keeps the browser hint.
func TestLaunchWorkspaceMintsPairedServicePrincipal(t *testing.T) {
	vaulttest.Use(t)
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = preflightTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"endpoints":[]}`))}, nil
	})
	t.Cleanup(func() { http.DefaultClient.Transport = old })
	for _, tc := range []mintRow{
		{"covers", "", "", true, 0, 50 * time.Minute, time.Hour, 0, "", 0},
		{"short, minted", "", "", true, 0, 10 * time.Minute, time.Hour, 1, "", 0},
		{"short, mint fails", "", "", true, 0, 10 * time.Minute, 0, 1, "databricks login --profile mint-row --as sp", 0},
		{"short, minted still short", "", "", true, 2 * time.Hour, 10 * time.Minute, time.Hour, 1, "token cannot cover", 0},
		{"no pairing", "", "", false, 0, 10 * time.Minute, time.Hour, 0, providers.LoginCommand("mint-row"), 0},
		{"identity user", providers.IdentityUser, "", true, 0, 10 * time.Minute, time.Hour, 0, providers.LoginCommand("mint-row"), 0},
		{"--as user", "", providers.IdentityUser, true, 0, 10 * time.Minute, time.Hour, 0, "token cannot cover", 0},
	} {
		t.Run(tc.name, tc.run)
	}
}

type mintRow struct {
	name, identity, as string
	paired             bool
	timeout, stored    time.Duration
	minted             time.Duration // the fresh token's life; 0 makes the mint fail
	wantMints          int
	wantFinding        string // substring of the token finding's detail; "" for none
	wantNotes          int
}

// seed stores the row's credentials and expiry records, returning the provider and a writer of the SP's expiry record.
func (tc mintRow) seed(t *testing.T) (*providers.Provider, func(name string, life time.Duration)) {
	sp := ""
	if tc.paired {
		sp = "sp-client"
	}
	p := &providers.Provider{Name: "mint-row", Provider: "databricks", Preset: providers.PresetDatabricks,
		Host: "https://mint.example.com", Identity: tc.identity, Auth: providers.Auth{Type: providers.AuthOAuth, SPClientID: sp}}
	if err := vault.Store(providers.AuthSecretName(p), "fixture"); err != nil {
		t.Fatal(err)
	}
	if tc.paired {
		if err := vault.Store(providers.SecretName("databricks", p.Name, providers.AuthM2M), "secret"); err != nil {
			t.Fatal(err)
		}
	}
	writes := 0
	record := func(name string, life time.Duration) {
		writes++
		dir := filepath.Join(paths.StateDir(), "cache", "token-expiry")
		raw := fmt.Sprintf(`{"schema":2,"exp":%d}`, time.Now().Add(life).Unix())
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, name+".json")
		if err := os.WriteFile(file, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		// Coarse fs clocks give back-to-back writes one mtime; the newer-record pick needs strict order.
		at := time.Now().Add(time.Duration(writes) * time.Second)
		if err := os.Chtimes(file, at, at); err != nil {
			t.Fatal(err)
		}
	}
	record(p.Name+".sp", tc.stored)
	record(p.Name, tc.stored)
	return p, record
}

func (tc mintRow) run(t *testing.T) {
	p, record := tc.seed(t)
	mints, oldMint := 0, mintPaired
	mintPaired = func(ctx context.Context, _ *providers.Provider) error {
		mints++
		if got := providers.IdentityFrom(ctx); got != providers.IdentityHeadless {
			t.Errorf("mint identity = %q, want headless", got)
		}
		if tc.minted == 0 {
			return errors.New("rejected")
		}
		record(p.Name+".sp", tc.minted)
		return nil
	}
	t.Cleanup(func() { mintPaired = oldMint })
	ctx := context.WithValue(context.Background(), runTimeoutKey{}, tc.timeout)
	if tc.as != "" {
		ctx = providers.WithIdentity(ctx, tc.as)
	}
	pf := &Preflight{}
	fs := launchWorkspaceFindings(ctx, "opencode", p, router.State{}, time.Now(), pf)
	if mints != tc.wantMints || len(pf.Notes) != tc.wantNotes {
		t.Errorf("mints = %d, notes = %v; want %d mints, %d notes", mints, pf.Notes, tc.wantMints, tc.wantNotes)
	}
	if tc.wantFinding == "" && len(fs) != 0 {
		t.Fatalf("findings %v, want none", fs)
	}
	if tc.wantFinding != "" && (len(fs) != 1 || fs[0].Check != "token" || !strings.Contains(fs[0].Detail, tc.wantFinding)) {
		t.Fatalf("findings %v, want one token finding containing %q", fs, tc.wantFinding)
	}
}

func TestPreflightKeepaliveExtendsOnlyExpiringAuth(t *testing.T) {
	for _, extend := range []bool{false, true} {
		t.Run(strconv.FormatBool(extend), func(t *testing.T) {
			vaulttest.Use(t)
			now := time.Now()
			hits := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				if !extend {
					w.WriteHeader(500)
					return
				}
				tok := hosttest.UnsignedJWT(fmt.Sprintf(`{"exp":%d}`, now.Add(time.Hour).Unix()))
				if err := json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "expires_in": 3600}); err != nil {
					t.Error(err)
				}
			}))
			defer srv.Close()
			p := &providers.Provider{Name: "gate-refresh", Provider: "databricks", BaseURL: srv.URL + "/serving-endpoints/anthropic", Auth: providers.Auth{Type: providers.AuthOAuth}}
			seedPreflightToken(t, p, now.Add(5*time.Minute))
			f := CheckToken(p, now, DefaultToolsTimeout)
			if (f == nil) != extend || hits != 1 {
				t.Fatalf("extend=%v finding=%v refresh requests=%d", extend, f, hits)
			}
			p.Auth.Type = providers.AuthPAT
			if f := CheckToken(p, now, 2*time.Hour); f != nil || hits != 1 {
				t.Fatalf("PAT touched: finding=%v requests=%d", f, hits)
			}
		})
	}
}

func seedPreflightToken(t *testing.T, p *providers.Provider, exp time.Time) {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"refresh": "fixture-refresh", "access": hosttest.UnsignedJWT(fmt.Sprintf(`{"exp":%d}`, exp.Unix()))})
	if err != nil {
		t.Fatal(err)
	}
	vaulttest.Secret(t, providers.AuthSecretName(p), string(raw))
	dir := filepath.Join(paths.StateDir(), "cache", "token-expiry")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	raw = []byte(fmt.Sprintf(`{"schema":2,"exp":%d,"method":"u2m"}`, exp.Unix()))
	if err := os.WriteFile(filepath.Join(dir, p.Name+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}
