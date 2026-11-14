package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/vault"
)

// loginRig is a user-only oauth row on a token server whose answer the test picks: dead (400 invalid_grant), reject
// (401), down (500) or ok (a one-hour JWT).
type loginRig struct {
	p    *Provider
	hits atomic.Int32
	mode atomic.Value // string
}

func newLoginRig(t *testing.T, name string) *loginRig {
	t.Helper()
	r := &loginRig{}
	r.mode.Store("dead")
	srv := oidcServer(t, func(*http.Request, url.Values) (int, map[string]any) {
		r.hits.Add(1)
		switch r.mode.Load().(string) {
		case "reject":
			return http.StatusUnauthorized, map[string]any{}
		case "down":
			return http.StatusInternalServerError, map[string]any{}
		case "ok":
			return http.StatusOK, map[string]any{"access_token": jwtExpIn(time.Hour, "https://x"), "expires_in": 3600}
		}
		return http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "refresh token expired"}
	})
	r.p = mintRow(t, name, AuthOAuth, "", srv.URL)
	r.p.Identity = IdentityUser
	putOAuth(t, r.p, oauthValue{Refresh: "refresh-1"})
	t.Cleanup(func() { loginMarks.Delete(name); loginNoted.Delete(name); cmdCache.Delete(name) })
	return r
}

// reopenWindow rewrites p's mark so its retry window has passed, as if 11 minutes went by.
func reopenWindow(t *testing.T, p *Provider) {
	t.Helper()
	rec, ok := readLoginMark(p.Name)
	if !ok {
		t.Fatal("no mark to age")
	}
	rec.Until = time.Now().Add(-time.Minute)
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(cacheRecordPath("login-state", p.Name), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDeadLoginWindowSuppressesTheNextRefresh(t *testing.T) {
	r := newLoginRig(t, "dead-window")
	ctx := context.Background()
	if _, err := Credential(ctx, r.p); err == nil || r.hits.Load() != 1 {
		t.Fatalf("first Credential: err %v, hits %d; want one failed refresh", err, r.hits.Load())
	}
	if st, dead := LoginStatus(r.p); !dead || st.State != "needs_login" || st.Profile != r.p.Name {
		t.Fatalf("LoginStatus = %+v, %v; want needs_login", st, dead)
	}
	_, err := Credential(ctx, r.p)
	assertDeadWindowError(t, err)
	if Verdict(err) != VerdictNeedsLogin || r.hits.Load() != 1 {
		t.Errorf("verdict %s, hits %d; want needs_login with no second token call", Verdict(err), r.hits.Load())
	}
	if _, err := Refresh(ctx, r.p); err == nil || r.hits.Load() != 2 {
		t.Errorf("forced Refresh: err %v, hits %d; an explicit refresh must still reach the token endpoint", err, r.hits.Load())
	}
}

// assertDeadWindowError requires err to name the dead-window profile and the login command that renews it.
func assertDeadWindowError(t *testing.T, err error) {
	t.Helper()
	want := "dead-window: your login needs renewal (needs_login since "
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "`claude-code databricks login --profile dead-window`") {
		t.Fatalf("second Credential error = %v; want it to name the profile and the login command", err)
	}
}

func TestDeadLoginMarkCrossesProcessesAndSuccessClearsIt(t *testing.T) {
	r := newLoginRig(t, "dead-cross")
	ctx := context.Background()
	r.mode.Store("reject")
	_, _ = Credential(ctx, r.p)
	if st, dead := LoginStatus(r.p); !dead || st.State != "rejected" {
		t.Fatalf("LoginStatus = %+v, %v; want rejected", st, dead)
	}
	loginMarks.Delete(r.p.Name) // a fresh process holds no in-memory state; the file carries the mark
	cmdCache.Delete(r.p.Name)
	if _, err := Credential(ctx, r.p); err == nil || r.hits.Load() != 1 {
		t.Fatalf("fresh process: err %v, hits %d; want the file's mark to suppress the refresh", err, r.hits.Load())
	}
	reopenWindow(t, r.p)
	r.mode.Store("ok")
	if _, err := Credential(ctx, r.p); err != nil || r.hits.Load() != 2 {
		t.Fatalf("after the window: err %v, hits %d; want one successful refresh", err, r.hits.Load())
	}
	if _, dead := LoginStatus(r.p); dead {
		t.Error("a successful mint left the dead mark")
	}
	if _, err := os.Stat(cacheRecordPath("login-state", r.p.Name)); !os.IsNotExist(err) {
		t.Errorf("mark file after success: %v; want it deleted", err)
	}
}

func TestDeadMarkKeepsSinceAcrossRetriesAndHoldsNoSecret(t *testing.T) {
	r := newLoginRig(t, "dead-since")
	_, _ = Credential(context.Background(), r.p)
	first, _ := LoginStatus(r.p)
	reopenWindow(t, r.p)
	_, _ = Credential(context.Background(), r.p)
	if again, _ := LoginStatus(r.p); !again.Since.Equal(first.Since) {
		t.Errorf("since moved from %v to %v on a repeat failure", first.Since, again.Since)
	}
	raw, err := os.ReadFile(cacheRecordPath("login-state", r.p.Name))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "schema,since,state,until" || strings.Contains(string(raw), "refresh") {
		t.Errorf("mark file = %s; want only schema, state, since, until", raw)
	}
}

func TestLoginStatusReadsNoVaultAndTreatsBadFilesAsMiss(t *testing.T) {
	r := newLoginRig(t, "dead-miss")
	_, _ = Credential(context.Background(), r.p)
	corruptVault(t)
	if _, dead := LoginStatus(r.p); !dead {
		t.Error("LoginStatus needed the vault")
	}
	path := cacheRecordPath("login-state", r.p.Name)
	for name, body := range map[string]string{
		"corrupt": "{not json", "newer schema": `{"schema":99,"state":"needs_login"}`, "empty state": `{"schema":1,"state":""}`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if st, dead := LoginStatus(r.p); dead {
			t.Errorf("%s file read as a mark %+v", name, st)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, dead := LoginStatus(r.p); dead {
		t.Error("an absent file read as a mark")
	}
	if got := (LoginState{Profile: "dev"}).Command(); got != "claude-code databricks login --profile dev" {
		t.Errorf("Command = %q", got)
	}
}

func TestProbeUserLoginMarksClearsAndSkipsTransient(t *testing.T) {
	r := newLoginRig(t, "dead-probe")
	ctx := context.Background()
	st, dead, err := ProbeUserLogin(ctx, r.p)
	if err != nil || !dead || st.State != "needs_login" || r.hits.Load() != 1 {
		t.Fatalf("probe of a dead login = %+v, %v, %v (hits %d)", st, dead, err, r.hits.Load())
	}
	assertTransientProbeKeepsMark(t, r)
	r.mode.Store("ok")
	if _, dead, err := ProbeUserLogin(ctx, r.p); err != nil || dead {
		t.Fatalf("probe of a healed login = dead %v, err %v", dead, err)
	}
	if _, still := LoginStatus(r.p); still {
		t.Error("a successful probe left the mark")
	}
	assertProbeWithoutLoginIsNoop(t, r)
}

// assertTransientProbeKeepsMark makes the endpoint fail transiently and requires an error with the mark unchanged.
func assertTransientProbeKeepsMark(t *testing.T, r *loginRig) {
	t.Helper()
	r.mode.Store("down")
	if _, dead, err := ProbeUserLogin(context.Background(), r.p); err == nil || dead {
		t.Errorf("transient probe = dead %v, err %v; want an error and no verdict", dead, err)
	}
	if st, _ := LoginStatus(r.p); st.State != "needs_login" {
		t.Errorf("a transient failure changed the mark to %+v", st)
	}
}

// assertProbeWithoutLoginIsNoop deletes the stored login and requires a probe to report nothing and skip the network.
func assertProbeWithoutLoginIsNoop(t *testing.T, r *loginRig) {
	t.Helper()
	if err := vault.Delete(AuthSecretName(r.p)); err != nil {
		t.Fatal(err)
	}
	hits := r.hits.Load()
	if _, dead, err := ProbeUserLogin(context.Background(), r.p); err != nil || dead || r.hits.Load() != hits {
		t.Errorf("probe with no stored login = dead %v, err %v, hits %d; want a no-op", dead, err, r.hits.Load()-hits)
	}
}

func TestFreshLoginClearsTheMark(t *testing.T) {
	r := newLoginRig(t, "dead-login")
	_, _ = Credential(context.Background(), r.p)
	if _, dead := LoginStatus(r.p); !dead {
		t.Fatal("no mark to clear")
	}
	if _, err := sealLogin(r.p, oauth.Token{Refresh: "refresh-2", Access: jwtExpIn(time.Hour, "")}); err != nil {
		t.Fatal(err)
	}
	if _, dead := LoginStatus(r.p); dead {
		t.Error("a fresh login left the dead mark")
	}
}

// An auto row goes straight to its paired SP while the dead login's window is open, and says so once.
func TestAutoGoesStraightToSPWhileWindowOpenAndReportsIt(t *testing.T) {
	p, m2m, refresh, note := spHarness(t, IdentityAuto)
	t.Cleanup(func() { loginMarks.Delete(p.Name); loginNoted.Delete(p.Name) })
	ctx := context.Background()
	if got, err := Credential(ctx, p); err != nil || got != "sp-token" || *refresh != 1 {
		t.Fatalf("first Credential = %q, %v (refresh %d)", got, err, *refresh)
	}
	spCache.Delete(p.Name)
	assertSPServedWithoutUserAttempt(t, p, m2m, refresh)
	assertFallbackNotedOnce(t, p, note.String())
}

// assertSPServedWithoutUserAttempt requires Credential and a forced Refresh to serve the SP with no new user refresh.
func assertSPServedWithoutUserAttempt(t *testing.T, p *Provider, m2m, refresh *int) {
	t.Helper()
	ctx := context.Background()
	if got, err := Credential(ctx, p); err != nil || got != "sp-token" || *refresh != 1 || *m2m != 2 {
		t.Fatalf("second Credential = %q, %v (refresh %d, mints %d); want the SP with no user attempt", got, err, *refresh, *m2m)
	}
	if got, err := Refresh(ctx, p); err != nil || got != "sp-token" || *refresh != 1 {
		t.Fatalf("forced Refresh = %q, %v (refresh %d); want the proxy's 401 replay to skip the dead login too", got, err, *refresh)
	}
}

// assertFallbackNotedOnce requires the SP-fallback note exactly once and no secret in the notes.
func assertFallbackNotedOnce(t *testing.T, p *Provider, notes string) {
	t.Helper()
	fallback := "user login needs renewal; using the service principal (run `claude-code databricks login --profile " + p.Name + "`)"
	if n := strings.Count(notes, fallback); n != 1 {
		t.Errorf("fallback reported %d times in %q; want once per window", n, notes)
	}
	if strings.Contains(notes, "sp-secret") {
		t.Errorf("note leaked the secret: %q", notes)
	}
}

// With the SP gone, a dead login's window still ends the request with the command named.
func TestAutoWithoutSPSecretNamesTheLoginCommand(t *testing.T) {
	p, _, refresh, _ := spHarness(t, IdentityAuto)
	t.Cleanup(func() { loginMarks.Delete(p.Name); loginNoted.Delete(p.Name) })
	if err := vault.Delete(SecretName(GroupDatabricks, p.Name, AuthM2M)); err != nil {
		t.Fatal(err)
	}
	if _, err := Credential(context.Background(), p); err == nil {
		t.Fatal("want the first refresh to fail")
	}
	_, err := Credential(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "run `claude-code databricks login --profile "+p.Name+"`") || *refresh != 1 {
		t.Fatalf("second Credential = %v (refresh %d); want the command with no new refresh", err, *refresh)
	}
}

// A vault read error is unknown, not absent: the dispatch tries the SP and does not skip it for spSkipWait.
func TestDispatchTreatsSPPresenceReadErrorAsUnknown(t *testing.T) {
	p, m2m, _, _ := spHarness(t, IdentityAuto)
	t.Cleanup(func() { spSkip.Delete(p.Name) })
	corruptVault(t)
	if present, err := spPresence(p); present || err == nil {
		t.Fatalf("spPresence = %v, %v; want a read error", present, err)
	}
	_, err := Credential(WithIdentity(context.Background(), IdentityHeadless), p)
	if _, skipped := spSkip.Load(p.Name); skipped || err == nil || *m2m != 0 {
		t.Fatalf("skipped=%v err=%v mints=%d; want an error from the SP try and no spSkip", skipped, err, *m2m)
	}
}
