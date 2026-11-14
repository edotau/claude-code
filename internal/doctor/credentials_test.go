package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

type credentialProbeTest struct {
	inflight, maxSeen atomic.Int32
	gate, over        chan struct{}
	once, overOnce    sync.Once
	byPtr             map[*providers.Provider]string
}

func (s *credentialProbeTest) probe(_ context.Context, p *providers.Provider) (string, string) {
	cur := s.inflight.Add(1)
	defer s.inflight.Add(-1)
	for {
		m := s.maxSeen.Load()
		if cur <= m || s.maxSeen.CompareAndSwap(m, cur) {
			break
		}
	}
	if cur >= credentialProbeWorkers {
		s.once.Do(func() { close(s.gate) })
	}
	if cur > credentialProbeWorkers {
		s.overOnce.Do(func() { close(s.over) })
	}
	select {
	case <-s.gate:
	case <-time.After(5 * time.Second): // only an implementation below the limit reaches this
	}
	select {
	case <-s.over:
	case <-time.After(100 * time.Millisecond): // gives a probe beyond the limit time to show up
	}
	n := s.byPtr[p]
	return "probe-" + n, "detail-" + n
}

func TestProviderCredentialChecksRunConcurrentlyInOrder(t *testing.T) {
	hermetic(t)
	names := []string{"p-0", "p-1", "p-2", "p-3", "p-4", "p-5", "p-6", "p-7"}
	reg := &providers.Registry{Default: "elsewhere", Providers: map[string]*providers.Provider{}}
	state := &credentialProbeTest{gate: make(chan struct{}), over: make(chan struct{}), byPtr: map[*providers.Provider]string{}}
	for _, n := range names {
		p := &providers.Provider{Auth: providers.Auth{Type: providers.AuthNone}}
		reg.Providers[n] = p
		state.byPtr[p] = n
	}
	old := probeCredential
	t.Cleanup(func() { probeCredential = old })
	probeCredential = state.probe
	rows := providerCredentialChecks(context.Background(), reg, providers.Selection{}, errors.New("no session"), names)
	assertCredentialProbeRows(t, rows, names)
	if m := state.maxSeen.Load(); m < 2 {
		t.Errorf("max in-flight probes = %d, want >= 2 (probes must overlap)", m)
	} else if m > credentialProbeWorkers {
		t.Errorf("max in-flight probes = %d, want <= %d (worker limit)", m, credentialProbeWorkers)
	}
}

func assertCredentialProbeRows(t *testing.T, rows []check, names []string) {
	t.Helper()
	if len(rows) != len(names) {
		t.Fatalf("got %d rows, want %d", len(rows), len(names))
	}
	for i, n := range names {
		if rows[i].Name != "credential "+n {
			t.Errorf("row %d = %q, want %q", i, rows[i].Name, "credential "+n)
		}
		if want := "probe-" + n + ": detail-" + n; !rows[i].OK || !strings.Contains(rows[i].Detail, want) {
			t.Errorf("row %s = %+v, want OK with detail containing %q", n, rows[i], want)
		}
	}
}

// A key-auth built-in nothing routes to (not session, default, fallback or pinned) is a soft note, not ✗;
// pinning it for an agent makes the missing key a real ✗ again.
func TestDoctorUnusedProviderMissingKeyIsSoft(t *testing.T) {
	doctorScratch(t)
	checks, _ := runChecks(context.Background())
	c := findCheck(checks, "credential openai")
	if c == nil || !c.OK || !strings.Contains(c.Detail, "unused") {
		t.Fatalf("unused openai row = %+v, want OK with an unused note", c)
	}
	vaulttest.SetRecord(t, "codex", "profile", "openai")
	checks, _ = runChecks(context.Background())
	if c := findCheck(checks, "credential openai"); c == nil || c.OK {
		t.Fatalf("pinned openai row = %+v, want ✗ for the missing key", c)
	}
}

// altAuthScratch isolates the machine state altAuth reads: CLAUDE_CONFIG_DIR (keyring/secrets.env),
// HOME (the gemini agy login marker, and the claude login probe has nowhere real to run), and a PATH with
// neither `claude` nor `security`/`secret-tool` on it, so the real Keychain and a real claude login
// can never answer for this test by accident.
func altAuthScratch(t *testing.T) (home string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("PATH", t.TempDir())
	home = filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv(providers.OAuthTokenEnv, "")
	return home
}

func TestAltAuthAnthropicHeadlessToken(t *testing.T) {
	altAuthScratch(t)
	t.Setenv(providers.OAuthTokenEnv, "sk-ant-oat-test")
	if got := altAuth("anthropic"); got != "claude uses the OAuth token instead" {
		t.Errorf("altAuth(anthropic) with a headless token = %q", got)
	}
}

func TestAltAuthAnthropicNone(t *testing.T) {
	altAuthScratch(t) // no headless token, no claude binary to report a login
	if got := altAuth("anthropic"); got != "" {
		t.Errorf("altAuth(anthropic) with neither = %q, want \"\"", got)
	}
}

// gemini has no altAuth case: GeminiLoginPresent (not altAuth) now reports the agy Google login.
func TestAltAuthGemini(t *testing.T) {
	home := altAuthScratch(t)
	if got := altAuth("gemini"); got != "" {
		t.Errorf("altAuth(gemini) without the agy marker = %q, want \"\"", got)
	}
	if err := os.MkdirAll(filepath.Join(home, ".gemini"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gemini", "jetski-standalone-oauth-token"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := altAuth("gemini"); got != "" {
		t.Errorf("altAuth(gemini) with the agy marker = %q, want \"\" (altAuth has no gemini case)", got)
	}
}

func TestAltAuthUnknownProvider(t *testing.T) {
	altAuthScratch(t)
	if got := altAuth("openai"); got != "" {
		t.Errorf("altAuth(openai) = %q, want \"\" (no alt auth path for this provider)", got)
	}
}

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
	for _, want := range []string{"verified", "warn on the service principal; your own login is not signed in (needs_login; to sign in: " +
		providers.LoginCommand("dbx") + ")"} {
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
