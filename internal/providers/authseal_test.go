package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
)

// rotRig is a u2m row whose token endpoint rotates the refresh token on every call (refresh-1 → refresh-2 → …) and
// runs onCall(n) just before answering call n.
type rotRig struct {
	p      *Provider
	mu     sync.Mutex
	sent   []string
	onCall func(n int)
}

func newRotRig(t *testing.T, name string) *rotRig {
	t.Helper()
	r := &rotRig{}
	srv := oidcServer(t, func(_ *http.Request, f url.Values) (int, map[string]any) {
		r.mu.Lock()
		r.sent = append(r.sent, f.Get("refresh_token"))
		n := len(r.sent)
		r.mu.Unlock()
		if r.onCall != nil {
			r.onCall(n)
		}
		return http.StatusOK, map[string]any{"access_token": jwtExpIn(time.Hour+time.Duration(n)*time.Second, "https://x"),
			"expires_in": 3600, "refresh_token": fmt.Sprintf("refresh-%d", n+1)}
	})
	r.p = mintRow(t, name, AuthOAuth, "", srv.URL)
	t.Cleanup(func() { cmdCache.Delete(name); unsealedRefresh.Delete(name) })
	putOAuth(t, r.p, oauthValue{Refresh: "refresh-1"})
	return r
}

func (r *rotRig) refreshesSent() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sent...)
}

// needReadOnlyVault skips a test that makes the vault file read-only where that is not enforced.
func needReadOnlyVault(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a read-only vault file is only enforced for a non-root unix user")
	}
}

// breakWrites makes the vault file read-only so a seal fails while reads still work; the returned func restores it.
// It runs on the token server's goroutine, so it reports with t.Error.
func breakWrites(t *testing.T) (restore func()) {
	path := vault.Path()
	if err := os.Chmod(path, 0o400); err != nil {
		t.Error(err)
	}
	restore = func() { _ = os.Chmod(path, 0o600) }
	t.Cleanup(restore)
	return restore
}

func sealFailures(t *testing.T) []audit.Event {
	t.Helper()
	evs, err := audit.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var out []audit.Event
	for _, e := range evs {
		if e.Action == audit.SealFailed {
			out = append(out, e)
		}
	}
	return out
}

func TestSealedStampWrittenAndOldRecordStillReads(t *testing.T) {
	p := mintRow(t, "stamp-row", AuthOAuth, "", "http://127.0.0.1:1")
	if err := vault.Store(AuthSecretName(p), `{"refresh":"old-r","access":"old-a"}`); err != nil {
		t.Fatal(err)
	}
	if ov, err := readOAuth(p); err != nil || ov.Refresh != "old-r" || ov.Access != "old-a" || ov.Sealed != 0 {
		t.Fatalf("record without a stamp = %+v, %v; want it read with Sealed 0", ov, err)
	}
	before := time.Now().UnixNano()
	putOAuth(t, p, oauthValue{Refresh: "new-r"})
	if ov, _ := readOAuth(p); ov.Sealed < before || ov.Sealed > time.Now().UnixNano() {
		t.Fatalf("Sealed = %d after a write at %d; want the write's unix nanos", ov.Sealed, before)
	}
}

// A rotated refresh whose seal fails leaves an audit trace (name and kind, never a value) and is retried next mint.
func TestFailedRotatedSealIsAuditedAndRetriedOnNextMint(t *testing.T) {
	needReadOnlyVault(t)
	r := newRotRig(t, "seal-audit")
	restore := breakWritesOnFirstCall(t, r)
	ctx := context.Background()
	if _, err := Refresh(ctx, r.p); err != nil {
		t.Fatalf("first mint = %v; want success despite the failed seal", err)
	}
	assertRefreshSealAudited(t, r)
	restore()
	if _, err := Refresh(ctx, r.p); err != nil {
		t.Fatalf("second mint = %v", err)
	}
	if got := r.refreshesSent(); len(got) != 2 || got[1] != "refresh-2" {
		t.Fatalf("refresh tokens sent = %q; want the carried refresh-2 retried on the next mint", got)
	}
	if ov, _ := readOAuth(r.p); ov.Refresh != "refresh-3" {
		t.Fatalf("sealed refresh = %q; want refresh-3 sealed by the retry", ov.Refresh)
	}
}

// breakWritesOnFirstCall makes the vault read-only from the rig's first token call; the returned func repairs it.
func breakWritesOnFirstCall(t *testing.T, r *rotRig) (restore func()) {
	t.Helper()
	repair := func() {}
	r.onCall = func(n int) {
		if n == 1 {
			repair = breakWrites(t)
		}
	}
	return func() { repair() }
}

// assertRefreshSealAudited checks the one refresh seal_failed event and that the audit log holds no token value.
func assertRefreshSealAudited(t *testing.T, r *rotRig) {
	t.Helper()
	evs := sealFailures(t)
	if len(evs) != 1 || evs[0].Resource != AuthSecretName(r.p) || evs[0].Detail != "refresh" {
		t.Fatalf("seal_failed events = %+v; want one naming %s with kind refresh", evs, AuthSecretName(r.p))
	}
	raw, _ := os.ReadFile(filepath.Join(paths.StateDir(), "audit.jsonl"))
	if strings.Contains(string(raw), "refresh-") {
		t.Fatalf("audit line carries a token value: %s", raw)
	}
}

// The access-token seal failing (u2m) is audited by secret name and kind.
func TestFailedAccessSealIsAudited(t *testing.T) {
	needReadOnlyVault(t)
	r := newRotRig(t, "seal-access")
	r.onCall = func(int) { breakWrites(t) }
	srvRefresh := refreshU2M
	refreshU2M = func(ctx context.Context, host, rt string) (oauth.Token, error) {
		tok, err := srvRefresh(ctx, host, rt)
		tok.Refresh = "" // the endpoint did not rotate: only the access token is sealed
		return tok, err
	}
	t.Cleanup(func() { refreshU2M = srvRefresh })
	if _, err := Refresh(context.Background(), r.p); err != nil {
		t.Fatal(err)
	}
	evs := sealFailures(t)
	if len(evs) != 1 || evs[0].Resource != AuthSecretName(r.p) || evs[0].Detail != "access" {
		t.Fatalf("seal_failed events = %+v; want one naming %s with kind access", evs, AuthSecretName(r.p))
	}
}

// A carried (unsealed) rotated refresh must not be replayed over a login another process sealed since.
func TestCarriedRefreshYieldsToNewerLogin(t *testing.T) {
	needReadOnlyVault(t)
	r := newRotRig(t, "carry-newer")
	restore := func() {}
	r.onCall = func(n int) {
		if n == 1 {
			restore = breakWrites(t)
		}
	}
	ctx := context.Background()
	if _, err := Refresh(ctx, r.p); err != nil {
		t.Fatal(err)
	}
	if _, ok := unsealedRefresh.Load(r.p.Name); !ok {
		t.Fatal("precondition: the failed seal must leave a carried refresh")
	}
	restore()
	putOAuth(t, r.p, oauthValue{Refresh: "login-refresh"}) // another process logs in
	if _, err := Refresh(ctx, r.p); err != nil {
		t.Fatal(err)
	}
	if got := r.refreshesSent(); got[1] != "login-refresh" {
		t.Fatalf("refresh tokens sent = %q; want the newer login's token, not the carried refresh-2", got)
	}
	if _, ok := unsealedRefresh.Load(r.p.Name); ok {
		t.Error("the stale carry survived a newer sealed record")
	}
	if ov, _ := readOAuth(r.p); ov.Refresh != "refresh-3" {
		t.Errorf("sealed refresh = %q; want the login chain's rotation refresh-3", ov.Refresh)
	}
}

// A rotation that lands after another process sealed a newer login must not overwrite that login.
func TestStaleRotationNeverOverwritesNewerLogin(t *testing.T) {
	r := newRotRig(t, "stale-rotation")
	r.onCall = func(int) { // another process's login lands while the mint is in flight
		if err := writeOAuth(r.p, oauthValue{Refresh: "login-refresh"}); err != nil {
			t.Error(err)
		}
	}
	if _, err := Refresh(context.Background(), r.p); err != nil {
		t.Fatal(err)
	}
	if ov, _ := readOAuth(r.p); ov.Refresh != "login-refresh" || ov.Access != "" {
		t.Fatalf("sealed record = refresh %q, access set %t; want the newer login untouched", ov.Refresh, ov.Access != "")
	}
	if _, ok := unsealedRefresh.Load(r.p.Name); ok {
		t.Error("a superseded rotation must not be carried")
	}
}

// End to end: a rotated refresh survives a failed seal and is used next; a later newer login wins over a later carry.
func TestRotatedRefreshSurvivesAndNewerLoginWins(t *testing.T) {
	needReadOnlyVault(t)
	r := newRotRig(t, "e2e-rotation")
	restore := func() {}
	r.onCall = func(n int) {
		if n == 1 || n == 3 {
			restore = breakWrites(t)
		}
	}
	ctx := context.Background()
	for range 3 {
		if _, err := Refresh(ctx, r.p); err != nil {
			t.Fatal(err)
		}
		restore()
		if len(r.refreshesSent()) == 2 {
			if ov, _ := readOAuth(r.p); ov.Refresh != "refresh-3" {
				t.Fatalf("after the retry sealed refresh = %q; want refresh-3", ov.Refresh)
			}
		}
	}
	putOAuth(t, r.p, oauthValue{Refresh: "login-refresh"})
	if _, err := Refresh(ctx, r.p); err != nil {
		t.Fatal(err)
	}
	want := []string{"refresh-1", "refresh-2", "refresh-3", "login-refresh"}
	if got := r.refreshesSent(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("refresh tokens sent = %q; want %q", got, want)
	}
	if ov, _ := readOAuth(r.p); ov.Refresh != "refresh-5" {
		t.Fatalf("sealed refresh = %q; want the login chain's refresh-5", ov.Refresh)
	}
}

// A forced refresh serves a valid token another process sealed instead of minting, unless it is the rejected one.
func TestForcedRefreshServesNewerSealedToken(t *testing.T) {
	p, hits, _ := u2mMint(t, "forced-reread")
	t.Cleanup(func() { cmdCache.Delete(p.Name) })
	ctx := context.Background()
	if _, err := Credential(ctx, p); err != nil || hits.Load() != 1 {
		t.Fatalf("Credential: hits %d, err %v", hits.Load(), err)
	}
	served, _ := cmdCache.Load(p.Name)
	cmdCache.Store(p.Name, cached{value: served.(cached).value, until: time.Now().Add(-time.Second)}) // past its cache window
	peer := jwtExpIn(2*time.Hour, "https://x")
	putAccess(t, p, peer) // another process sealed a newer token
	if got, err := Refresh(ctx, p); err != nil || got != peer || hits.Load() != 1 {
		t.Fatalf("Refresh = %q, %v (hits %d); want the peer's sealed token with no mint", got, err, hits.Load())
	}
	if _, err := Refresh(ctx, p); err != nil || hits.Load() != 2 {
		t.Fatalf("second Refresh: hits %d, err %v; want a mint once the sealed token is the rejected one", hits.Load(), err)
	}
	cmdCache.Delete(p.Name)
	putAccess(t, p, peer)
	if _, err := Refresh(ctx, p); err != nil || hits.Load() != 3 {
		t.Fatalf("Refresh with nothing served: hits %d, err %v; want a mint as before", hits.Load(), err)
	}
}

func TestForcedSPRefreshServesNewerSealedToken(t *testing.T) {
	p, _, _, _ := spHarness(t, IdentitySP)
	mints := countingM2M(t, jwtExpIn(time.Hour, ""))
	ctx := context.Background()
	first, err := Credential(ctx, p)
	if err != nil || *mints != 1 {
		t.Fatalf("Credential = %q, %v (mints %d)", first, err, *mints)
	}
	peer := jwtExpIn(2*time.Hour, "")
	b, _ := json.Marshal(clientAccess{Access: peer, Bind: clientBind(AuthHost(p), "sp-client", "sp-secret")})
	if err := vault.Store(SecretName(GroupDatabricks, p.Name, AuthM2M)+"_ACCESS", string(b)); err != nil {
		t.Fatal(err)
	}
	if got, err := Refresh(ctx, p); err != nil || got != peer || *mints != 1 {
		t.Fatalf("Refresh = %q, %v (mints %d); want the peer's sealed SP token with no mint", got, err, *mints)
	}
	if _, err := Refresh(ctx, p); err != nil || *mints != 2 {
		t.Fatalf("second Refresh: mints %d, err %v; want a mint once the sealed token is the rejected one", *mints, err)
	}
}

// countingM2M replaces the SP mint with one that returns tok and counts its calls.
func countingM2M(t *testing.T, tok string) *int {
	t.Helper()
	n, orig := new(int), mintM2M
	mintM2M = func(context.Context, string, string, string) (oauth.Token, error) {
		*n++
		return oauth.Token{Access: tok}, nil
	}
	t.Cleanup(func() { mintM2M = orig })
	return n
}

// holdRefreshLock holds p's refresh lock until the returned func is called.
func holdRefreshLock(t *testing.T, p *Provider) (release func()) {
	t.Helper()
	held, done, stop := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_ = withRefreshLock(p, func() error { close(held); <-stop; return nil })
	}()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("could not take the refresh lock")
	}
	var once sync.Once
	release = func() { once.Do(func() { close(stop); <-done }) }
	t.Cleanup(release)
	return release
}

// An SP mint that waited on the lock serves the token the winner sealed instead of minting again.
func TestSPMintFollowerServesWinnersToken(t *testing.T) {
	p, _, _, _ := spHarness(t, IdentitySP)
	mints := countingM2M(t, jwtExpIn(time.Hour, ""))
	release := holdRefreshLock(t, p)
	type result struct {
		v   string
		err error
	}
	done := make(chan result, 1)
	go func() { v, _, err := mintSP(context.Background(), p, false, expiryMargin); done <- result{v, err} }()
	time.Sleep(100 * time.Millisecond)
	winner := jwtExpIn(time.Hour, "")
	b, _ := json.Marshal(clientAccess{Access: winner, Bind: clientBind(AuthHost(p), "sp-client", "sp-secret")})
	if err := vault.Store(SecretName(GroupDatabricks, p.Name, AuthM2M)+"_ACCESS", string(b)); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case r := <-done:
		if r.err != nil || r.v != winner || *mints != 0 {
			t.Fatalf("mintSP = %q, %v (mints %d); want the winner's sealed token with no mint", r.v, r.err, *mints)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("mintSP never returned")
	}
}

// A lock timeout never fails an SP mint: it proceeds unlocked.
func TestSPMintLockTimeoutMintsUnlocked(t *testing.T) {
	p, _, _, _ := spHarness(t, IdentitySP)
	tok := jwtExpIn(time.Hour, "")
	mints := countingM2M(t, tok)
	holdRefreshLock(t, p)
	defer func(d time.Duration) { refreshLockWait = d }(refreshLockWait)
	refreshLockWait = 50 * time.Millisecond
	got, _, err := mintSP(context.Background(), p, false, expiryMargin)
	if err != nil || got != tok || *mints != 1 {
		t.Fatalf("mintSP under a held lock = %q, %v (mints %d); want one unlocked mint", got, err, *mints)
	}
}

// A read that times out on the vault lock is retried once.
func TestReadOAuthRetriesOneVaultLockTimeout(t *testing.T) {
	p := mintRow(t, "read-retry", AuthOAuth, "", "http://127.0.0.1:1")
	putOAuth(t, p, oauthValue{Refresh: "refresh-1"})
	defer vault.SetReadLockTimeoutForTest(300 * time.Millisecond)()
	held, release := make(chan struct{}), make(chan struct{})
	writer := make(chan error, 1)
	go func() {
		writer <- vault.Update(func(*vault.Tx) error { close(held); <-release; return nil })
	}()
	<-held
	time.AfterFunc(450*time.Millisecond, func() { close(release) }) // after the first attempt's 300ms, inside the retry's
	ov, err := readOAuth(p)
	if err != nil || ov.Refresh != "refresh-1" {
		t.Fatalf("readOAuth = %+v, %v; want the retry to read once the writer is done", ov, err)
	}
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
}

// A vault error on the user path serves the paired SP, for an interactive auto request and for a dispatch.
func TestVaultErrorOnUserPathServesPairedSP(t *testing.T) {
	for _, headless := range []bool{false, true} {
		name := map[bool]string{false: "auto", true: "dispatch"}[headless]
		t.Run(name, func(t *testing.T) {
			p, m2m, _, note := spHarness(t, IdentityAuto)
			t.Setenv(SecretName(GroupDatabricks, p.Name, AuthM2M), "sp-secret") // the SP secret does not need the vault
			corruptVault(t)
			ctx := context.Background()
			if headless {
				ctx = WithIdentity(ctx, IdentityHeadless)
				spSkip.Store(p.Name, time.Now().Add(time.Hour))
				t.Cleanup(func() { spSkip.Delete(p.Name) })
			}
			got, err := Credential(ctx, p)
			if err != nil || got != "sp-token" || *m2m != 1 {
				t.Fatalf("Credential = %q, %v (mints %d); want the SP token after the vault error", got, err, *m2m)
			}
			if !strings.Contains(note.String(), "vault") {
				t.Errorf("note = %q; want it to name the vault failure", note.String())
			}
			evs := sealFailures(t)
			if len(evs) != 1 || evs[0].Detail != "sp-access" || !strings.HasSuffix(evs[0].Resource, "_M2M_ACCESS") {
				t.Errorf("seal_failed events = %+v; want one sp-access for the unwritable slot", evs)
			}
		})
	}
}

// Without a paired SP a vault error is returned, not hidden.
func TestVaultErrorWithoutPairedSPIsReturned(t *testing.T) {
	p, m2m, _, _ := spHarness(t, IdentityAuto)
	p.Auth.SPClientID = ""
	corruptVault(t)
	if got, err := Credential(context.Background(), p); err == nil || !strings.Contains(err.Error(), "vault:") || *m2m != 0 {
		t.Fatalf("Credential = %q, %v (mints %d); want the vault error and no SP mint", got, err, *m2m)
	}
}

// corruptVault makes every vault read and write fail with a database error.
func corruptVault(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(vault.Path(), []byte("not a bolt database"), 0o600); err != nil {
		t.Fatal(err)
	}
}
