package providers

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestRefreshedCommandOutranksExpiredEnv(t *testing.T) {
	p := freshProvider(t, &Provider{Name: "refresh-env-order", Auth: Auth{Type: AuthKey, Env: "K", Command: "echo fresh-token"}})
	t.Setenv("K", "expired")
	for _, step := range []struct {
		name string
		call func(context.Context, *Provider) (string, error)
		want string
	}{
		{"initial static credential", Credential, "expired"},
		{"forced refresh", Refresh, "fresh-token"},
		{"credential after refresh", Credential, "fresh-token"},
	} {
		if got, err := step.call(context.Background(), p); err != nil || got != step.want {
			t.Fatalf("%s = %q, %v; want %q", step.name, got, err, step.want)
		}
	}
}

func TestRedactDetailQuotedJSONToken(t *testing.T) {
	if got := redactDetail(`{"access_token": "eyJabc.def.ghi"}`); got != `{"access_token": "REDACTED"}` {
		t.Fatalf("quoted JSON redaction = %q; want token masked with JSON punctuation preserved", got)
	}
}

// joiningContext signals only after a follower has found the flight and entered its wait select.
type joiningContext struct {
	context.Context
	joined chan struct{}
	once   sync.Once
}

func (c *joiningContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.joined) })
	return c.Context.Done()
}

func waitFlightSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for flight signal")
	}
}

func TestForcedFlightPanicFailsJoinedFollower(t *testing.T) {
	for _, kind := range []string{"command", "sp"} {
		t.Run(kind, func(t *testing.T) { checkPanicFollower(t, kind) })
	}
}

func panicFlightCall(t *testing.T, kind string, mint func(context.Context) (string, error)) func(context.Context) (string, error) {
	t.Helper()
	if kind == "command" {
		p := &Provider{Name: "forced-panic-command"}
		return func(ctx context.Context) (string, error) { return mintOnce(ctx, p, true, mint) }
	}
	p, _, _, _ := spHarness(t, IdentitySP)
	orig := mintM2M
	mintM2M = func(ctx context.Context, _, _, _ string) (oauth.Token, error) {
		_, err := mint(ctx)
		return oauth.Token{}, err
	}
	t.Cleanup(func() { mintM2M = orig })
	return func(ctx context.Context) (string, error) { return spCredential(ctx, p, true) }
}

func checkPanicFollower(t *testing.T, kind string) {
	t.Helper()
	started, release, recovered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	releaseLeader := func() { unblock.Do(func() { close(release) }) }
	defer releaseLeader()
	call := panicFlightCall(t, kind, func(context.Context) (string, error) {
		close(started)
		<-release
		panic("leader mint failed")
	})
	go func() {
		defer close(recovered)
		defer func() { _ = recover() }()
		_, _ = call(context.Background())
	}()
	waitFlightSignal(t, started)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	follower := &joiningContext{Context: ctx, joined: make(chan struct{})}
	done := make(chan struct{})
	var got string
	var err error
	go func() { got, err = call(follower); close(done) }()
	waitFlightSignal(t, follower.joined)
	releaseLeader()
	waitFlightSignal(t, recovered)
	waitFlightSignal(t, done)
	if got != "" || !errors.Is(err, errMintAborted) {
		t.Fatalf("joined follower after leader panic = %q, %v; want empty token and mint-aborted error", got, err)
	}
}

func TestCredentialPresentOAuthSPWithOnlyM2MSecret(t *testing.T) {
	vaulttest.Use(t)
	p := &Provider{Name: "paired-presence", Provider: GroupDatabricks, Identity: IdentitySP,
		Auth: Auth{Type: AuthOAuth, SPClientID: "paired-client"}}
	t.Setenv(AuthSecretName(p), "")
	m2mName := SecretName(p.Provider, p.Name, AuthM2M)
	t.Setenv(m2mName, "")
	if CredentialPresent(p) {
		t.Fatal("SP present before its M2M secret was stored")
	}
	vaulttest.Secret(t, m2mName, "paired-client-secret")
	if vaultHas(t, AuthSecretName(p)) {
		t.Fatal("test unexpectedly seeded an OAuth secret")
	}
	if !CredentialPresent(p) {
		t.Fatal("OAuth SP with client ID and profile M2M secret reported absent")
	}
}

type authFixFlightResult struct {
	value string
	err   error
}

func authFixStartFlight(g *flightGroup, ctx context.Context, mint func(context.Context) (string, error)) <-chan authFixFlightResult {
	done := make(chan authFixFlightResult, 1)
	go func() {
		v, err := g.do(ctx, "cancel-regression", true, mint)
		done <- authFixFlightResult{v, err}
	}()
	return done
}

func authFixWaitResult(t *testing.T, done <-chan authFixFlightResult) authFixFlightResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("flight caller did not stop waiting")
		return authFixFlightResult{}
	}
}

func TestFlightGroupCanceledLeaderStopsWaitingWhileFollowerMints(t *testing.T) {
	g := &flightGroup{flights: map[string]*flight{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	mintContext := make(chan context.Context, 1)
	leader := authFixStartFlight(g, ctx, func(ctx context.Context) (string, error) {
		mintContext <- ctx
		close(started)
		<-release
		return "shared-result", ctx.Err()
	})
	waitFlightSignal(t, started)
	fctx, stopFollower := context.WithTimeout(context.Background(), 15*time.Second)
	defer stopFollower()
	joining := &joiningContext{Context: fctx, joined: make(chan struct{})}
	follower := authFixStartFlight(g, joining, func(context.Context) (string, error) {
		return "unexpected-second-mint", nil
	})
	waitFlightSignal(t, joining.joined)
	cancel()
	result := authFixWaitResult(t, leader)
	if !errors.Is(result.err, context.Canceled) || result.value != "" {
		t.Fatalf("canceled leader returned value=%t err=%v", result.value != "", result.err)
	}
	mintCtx := <-mintContext
	if mintCtx.Err() != nil || mintCtx.Done() != nil {
		t.Fatal("leader cancellation reached the shared mint context")
	}
	unblock()
	result = authFixWaitResult(t, follower)
	if result.err != nil || result.value != "shared-result" {
		t.Fatalf("follower did not receive the original mint: err=%v", result.err)
	}
}

// staleRig is a u2m row on a stubbed token endpoint that counts calls, records each refresh token spent and rotates it.
type staleRig struct {
	p       *Provider
	calls   atomic.Int32
	mu      sync.Mutex
	spent   []string
	clock   atomic.Int64
	fail    error
	gate    chan struct{}
	entered chan struct{}
	hook    func()        // runs once the refresh token is spent, before the reply
	life    time.Duration // minted lifetime; 0 is one hour
	opaque  bool          // mint a non-JWT token with no expires_in
}

// jwtLife is an unsigned JWT issued at iat that expires at exp.
func jwtLife(iat, exp time.Time) string {
	return hosttest.UnsignedJWT(fmt.Sprintf(`{"iss":"","iat":%d,"exp":%d}`, iat.Unix(), exp.Unix()))
}

// newStaleRig seals and caches a one-hour token with left to live; async turns on BackgroundRefresh.
func newStaleRig(t *testing.T, name string, left time.Duration, async bool) (*staleRig, string) {
	t.Helper()
	r := &staleRig{p: mintRow(t, name, AuthOAuth, "", "https://h.example.invalid"), entered: make(chan struct{}, 1)}
	r.clock.Store(time.Now().UnixNano())
	origNow, origRefresh := credNow, refreshU2M
	credNow, refreshU2M = r.now, r.refresh
	t.Cleanup(func() {
		credNow, refreshU2M = origNow, origRefresh
		cmdCache.Delete(name)
		cmdFlights.mu.Lock()
		delete(cmdFlights.tried, name)
		cmdFlights.mu.Unlock()
	})
	old := jwtLife(r.now().Add(left-time.Hour), r.now().Add(left))
	putOAuth(t, r.p, oauthValue{Refresh: "refresh-1", Access: old})
	if err := finishCredential(r.p, old, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if async {
		t.Cleanup(BackgroundRefresh(context.Background()))
	}
	return r, old
}

func (r *staleRig) now() time.Time          { return time.Unix(0, r.clock.Load()) }
func (r *staleRig) advance(d time.Duration) { r.clock.Add(int64(d)) }
func (r *staleRig) spentTokens() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.spent...)
}
func (r *staleRig) credential() (string, error) { return Credential(context.Background(), r.p) }

func (r *staleRig) refresh(ctx context.Context, _, refresh string) (oauth.Token, error) {
	n := r.calls.Add(1)
	r.mu.Lock()
	r.spent = append(r.spent, refresh)
	r.mu.Unlock()
	select {
	case r.entered <- struct{}{}:
	default:
	}
	if r.hook != nil {
		r.hook()
	}
	if r.gate != nil {
		<-r.gate
	}
	// a real client drops the reply once its context ends, after the server spent the refresh token
	if err := ctx.Err(); err != nil {
		return oauth.Token{}, err
	}
	if r.fail != nil {
		return oauth.Token{}, r.fail
	}
	return r.minted(n), nil
}

func (r *staleRig) minted(n int32) oauth.Token {
	now, life := r.now(), cmp.Or(r.life, time.Hour)
	if r.opaque {
		return oauth.Token{Access: fmt.Sprintf("opaque-%d", n), Refresh: fmt.Sprintf("refresh-%d", n+1)}
	}
	return oauth.Token{Access: jwtLife(now, now.Add(life)), Refresh: fmt.Sprintf("refresh-%d", n+1), Expiry: now.Add(life)}
}

// waitBackground waits for name's in-flight mint, if any, to finish.
func waitBackground(t *testing.T, name string) {
	t.Helper()
	cmdFlights.mu.Lock()
	f := cmdFlights.flights[name]
	cmdFlights.mu.Unlock()
	if f == nil {
		return
	}
	select {
	case <-f.done:
	case <-time.After(5 * time.Second):
		t.Fatal("background refresh did not finish")
	}
}

func TestFreshTokenServedWithoutRefresh(t *testing.T) {
	r, old := newStaleRig(t, "stale-fresh", 50*time.Minute, true)
	for range 10 {
		if v, err := r.credential(); err != nil || v != old {
			t.Fatalf("fresh call: same=%t err=%v", v == old, err)
		}
	}
	waitBackground(t, r.p.Name)
	if n := r.calls.Load(); n != 0 {
		t.Fatalf("fresh token refreshed %d times, want 0", n)
	}
}

func TestStaleTokenServedWhileOneRefreshRuns(t *testing.T) {
	r, old := newStaleRig(t, "stale-herd", 15*time.Minute, true)
	r.gate = make(chan struct{})
	vals, errs := herd(r.p, 20)
	for i := range vals {
		if errs[i] != nil || vals[i] != old {
			t.Fatalf("caller %d: served the cached token=%t err=%v; want it at once", i, vals[i] == old, errs[i])
		}
	}
	close(r.gate)
	waitBackground(t, r.p.Name)
	got, err := r.credential()
	if err != nil || got == old || r.calls.Load() != 1 {
		t.Fatalf("after the refresh: new=%t calls=%d err=%v; want one refresh and its token", got != old, r.calls.Load(), err)
	}
	if ov, _ := readOAuth(r.p); ov.Refresh != "refresh-2" || ov.Access != got {
		t.Fatalf("vault: rotated=%t access sealed=%t", ov.Refresh == "refresh-2", ov.Access == got)
	}
	checkPeerAdoptsRotation(t, r, old, got)
}

// checkPeerAdoptsRotation: a peer caching the stale token adopts the sealed rotation instead of spending it again.
func checkPeerAdoptsRotation(t *testing.T, r *staleRig, old, got string) {
	t.Helper()
	r.advance(refreshCooldown + time.Second)
	if err := finishCredential(r.p, old, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if v, err := r.credential(); err != nil || v != old {
		t.Fatalf("peer stale call: same=%t err=%v", v == old, err)
	}
	waitBackground(t, r.p.Name)
	if v, _ := r.credential(); v != got || len(r.spentTokens()) != 1 || r.spentTokens()[0] != "refresh-1" {
		t.Fatalf("peer: adopted=%t refresh tokens spent=%d; want the sealed token and one spend", v == got, len(r.spentTokens()))
	}
}

func TestStaleOrExpiredTokenBlocksOnRefresh(t *testing.T) {
	for _, c := range []struct {
		name  string
		left  time.Duration
		async bool
	}{
		{"stale in a short-lived process", 15 * time.Minute, false},
		{"inside the expiry skew", 30 * time.Second, true},
		{"expired", -time.Minute, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, old := newStaleRig(t, "stale-block", c.left, c.async)
			if v, err := r.credential(); err != nil || v == old || r.calls.Load() != 1 {
				t.Fatalf("got the old token=%t calls=%d err=%v; want a blocking refresh", v == old, r.calls.Load(), err)
			}
		})
	}
}

func TestFailedBackgroundRefreshCoolsDown(t *testing.T) {
	r, old := newStaleRig(t, "stale-fail", 10*time.Minute, true)
	putOAuth(t, r.p, oauthValue{Refresh: "refresh-1"})
	r.fail = errors.New("token endpoint: connection reset")
	burst := func(step time.Duration, wantCalls int32) {
		t.Helper()
		r.advance(step)
		for range 50 {
			if v, err := r.credential(); err != nil || v != old {
				t.Fatalf("stale call: served=%t err=%v; want the valid cached token", v == old, err)
			}
		}
		waitBackground(t, r.p.Name)
		if n := r.calls.Load(); n != wantCalls {
			t.Fatalf("refresh attempts = %d, want %d", n, wantCalls)
		}
	}
	burst(0, 1)
	burst(refreshCooldown/2, 1)
	burst(refreshCooldown/2+time.Second, 2)
	r.advance(10*time.Minute - refreshCooldown - time.Second - expirySkew/2)
	if v, err := r.credential(); err == nil || v != "" || r.calls.Load() != 3 {
		t.Fatalf("past the skew: value=%t calls=%d err=%v; want the refresh error", v != "", r.calls.Load(), err)
	}
}

// stop (or a cancelled caller) arriving after R1 was sent: the reply is still read and R2 sealed before stop returns.
func TestSentExchangeSealsDespiteStop(t *testing.T) {
	r, old := newStaleRig(t, "stale-stop", 15*time.Minute, false)
	r.gate = make(chan struct{})
	stop := BackgroundRefresh(context.Background())
	t.Cleanup(stop)
	if v, err := r.credential(); err != nil || v != old {
		t.Fatalf("stale call: same=%t err=%v", v == old, err)
	}
	waitSignal(t, r.entered)
	stopped := make(chan string, 1)
	go func() { stop(); ov, _ := readOAuth(r.p); stopped <- ov.Refresh }()
	select {
	case got := <-stopped:
		t.Fatalf("stop returned before the sent exchange finished (sealed %q)", got)
	case <-time.After(100 * time.Millisecond):
	}
	close(r.gate)
	select {
	case got := <-stopped:
		if got != "refresh-2" || strings.Join(r.spentTokens(), ",") != "refresh-1" {
			t.Fatalf("after stop: sealed %q, spent %q; want refresh-2 sealed and refresh-1 sent once", got, r.spentTokens())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not return after the seal")
	}
}

func TestCancelledCallerStillSealsSentExchange(t *testing.T) {
	r, _ := newStaleRig(t, "cancel-sent", -time.Minute, false)
	ctx, cancel := context.WithCancel(context.Background())
	r.hook = cancel
	if _, _, err := lockedUserMint(ctx, r.p, false, freshMargin); err != nil {
		t.Fatal(err)
	}
	if ov, _ := readOAuth(r.p); ov.Refresh != "refresh-2" {
		t.Fatalf("sealed %q; want refresh-2 although the caller cancelled mid-exchange", ov.Refresh)
	}
}

func waitSignal(t *testing.T, c <-chan struct{}) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(5 * time.Second):
		t.Fatal("no refresh started")
	}
}

func TestRotatedSealRetriedThenSurfaced(t *testing.T) {
	defer func(d time.Duration) { sealRetryDelay = d }(sealRetryDelay)
	sealRetryDelay = time.Millisecond
	for _, c := range []struct {
		name     string
		failures int32
	}{{"fails once", 1}, {"never saves", 100}} {
		t.Run(c.name, func(t *testing.T) {
			r, _ := newStaleRig(t, "seal-retry", -time.Minute, false)
			var writes atomic.Int32
			orig := vaultStore
			vaultStore = func(name, v string) error {
				if writes.Add(1) <= c.failures {
					return errors.New("vault: database is read-only")
				}
				return orig(name, v)
			}
			t.Cleanup(func() { vaultStore = orig; unsealedRefresh.Delete(r.p.Name) })
			_, err := r.credential()
			checkUnsaved(t, err, c.failures > 1)
			if again, err := r.credential(); err != nil || again == "" || r.calls.Load() != 1 {
				t.Fatalf("next call: served=%t calls=%d err=%v; want the in-memory token", again != "", r.calls.Load(), err)
			}
		})
	}
}

// checkUnsaved wants the unsaved-rotation error when unsaved, else success.
func checkUnsaved(t *testing.T, err error, unsaved bool) {
	t.Helper()
	if unsaved && !errors.Is(err, errRotationUnsaved) || !unsaved && err != nil {
		t.Fatalf("mint: %v; want the unsaved-rotation error=%t", err, unsaved)
	}
}

func TestShortLivedPeerTokenNotAdopted(t *testing.T) {
	now := time.Now()
	short := jwtLife(now.Add(-time.Minute), now.Add(30*time.Second))
	if _, ok := jwtValidPast(short, freshMargin); ok {
		t.Fatal("a 90s token with 30s left was adopted")
	}
	if e := cacheEntry(&Provider{Auth: Auth{Type: AuthOAuth}}, short, now.Add(time.Hour), now.Add(30*time.Second), now.Add(-time.Minute)); e.until.After(e.hard) {
		t.Fatalf("cache entry fresh until %v past its serve limit %v", e.until, e.hard)
	}
}

func TestCancelledLockWaitSendsNoExchange(t *testing.T) {
	r, _ := newStaleRig(t, "lock-cancel", -time.Minute, false)
	lock := filepath.Join(paths.StateDir(), "locks", "auth-lock-cancel.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		t.Fatal(err)
	}
	held, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release) })
	go func() {
		_, _ = paths.TryFileLock(lock, time.Second, func() error { close(held); <-release; return nil })
	}()
	waitSignal(t, held)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, err := lockedUserMint(ctx, r.p, false, freshMargin); done <- err }()
	cancel()
	select {
	case err := <-done:
		if err == nil || r.calls.Load() != 0 {
			t.Fatalf("err=%v exchanges=%d; want a cancelled wait and no exchange", err, r.calls.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled lock wait kept waiting")
	}
}

func TestCooldownStartsWhenAttemptEnds(t *testing.T) {
	r, old := newStaleRig(t, "cool-finish", 15*time.Minute, true)
	putOAuth(t, r.p, oauthValue{Refresh: "refresh-1"})
	r.fail, r.gate = errors.New("token endpoint: connection reset"), make(chan struct{})
	if v, _ := r.credential(); v != old {
		t.Fatal("stale call did not serve the cached token")
	}
	waitSignal(t, r.entered)
	r.advance(refreshCooldown - time.Second)
	close(r.gate)
	waitBackground(t, r.p.Name)
	for _, step := range []struct {
		advance time.Duration
		want    int32
	}{{2 * time.Second, 1}, {refreshCooldown, 2}} {
		r.advance(step.advance)
		_, _ = r.credential()
		waitBackground(t, r.p.Name)
		if n := r.calls.Load(); n != step.want {
			t.Fatalf("after +%v: attempts = %d, want %d", step.advance, n, step.want)
		}
	}
}

// A lifetime under the skew still caches until its own skew; an opaque token caches for the row's ttl.
func TestShortOrUnknownLifetimeRefreshesOnce(t *testing.T) {
	for _, c := range []struct {
		name   string
		life   time.Duration
		opaque bool
	}{{"50s lifetime", 50 * time.Second, false}, {"no expiry", 0, true}} {
		t.Run(c.name, func(t *testing.T) {
			r, _ := newStaleRig(t, "short-life", -time.Minute, false)
			r.life, r.opaque = c.life, c.opaque
			for range 20 {
				if _, err := r.credential(); err != nil {
					t.Fatal(err)
				}
			}
			if n := r.calls.Load(); n != 1 {
				t.Fatalf("20 calls made %d refreshes, want 1", n)
			}
		})
	}
}

func TestCacheNeverServesPastExpirySkew(t *testing.T) {
	var m sync.Map
	now := time.Now()
	// a staleServe entry can stay fresh past its token's skew point; the skew wins
	m.Store("k", cached{value: "v", until: now.Add(time.Minute), hard: now.Add(-time.Second)})
	if v, st := cacheLookup(&m, "k"); st != cacheMiss || v != "" {
		t.Fatalf("state %d served=%t; want a miss past the skew", st, v != "")
	}
}

func TestStaleServeLogsClassOnly(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	var cache sync.Map
	_, served := staleServe(&Provider{Name: "log-class"}, &cache, "refresh", "old", errors.New("upstream said refresh_token=leaky-value"))
	os.Stderr = orig
	_ = w.Close()
	out, _ := io.ReadAll(r)
	if !served || strings.Contains(string(out), "leaky") || !strings.Contains(string(out), "log-class: refresh failed (transient)") {
		t.Fatalf("served=%t log %q; want one classified line with no upstream text", served, out)
	}
}
