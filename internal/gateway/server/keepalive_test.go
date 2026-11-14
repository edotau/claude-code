package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const keepaliveOverlay = `{"default":"hot","providers":{"databricks":{` +
	`"hot":{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"oauth"},"models":{"opus":"m"}},` +
	`"idle":{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"oauth"},"models":{"opus":"m"}},` +
	`"stale":{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"oauth"},"models":{"opus":"m"}}}}}`

// kaRig is a router whose registry, clock, log and refresh are fakes; calls records each refreshed profile in order.
type kaRig struct {
	r     *Router
	log   *lockedBuf
	mu    sync.Mutex
	now   time.Time
	calls []string
	fn    func(ctx context.Context, p *providers.Provider) (bool, error)
}

func newKARig(t *testing.T) *kaRig {
	t.Helper()
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", keepaliveOverlay)
	g := &kaRig{now: time.Unix(1_700_000_000, 0), log: &lockedBuf{}}
	g.r = newRouter(secret)
	g.r.log = g.log
	g.r.now = func() time.Time { g.mu.Lock(); defer g.mu.Unlock(); return g.now }
	g.r.ka.refresh = func(ctx context.Context, p *providers.Provider) (bool, error) {
		g.mu.Lock()
		g.calls = append(g.calls, p.Name)
		fn := g.fn
		g.mu.Unlock()
		if fn != nil {
			return fn(ctx, p)
		}
		return true, nil
	}
	return g
}

func (g *kaRig) called() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

func TestKeepaliveTickRefreshesOnlyRecentlyUsedProfiles(t *testing.T) {
	g := newKARig(t)
	g.r.touch("stale")
	g.mu.Lock()
	g.now = g.now.Add(keepaliveWindow + time.Minute) // "stale" is now past the window
	g.mu.Unlock()
	g.r.touch("hot")
	g.r.keepaliveTick(t.Context())
	if got := g.called(); len(got) != 1 || got[0] != "hot" {
		t.Fatalf("refreshed %v, want only hot (idle never used, stale past %s)", got, keepaliveWindow)
	}
	if !strings.Contains(g.log.String(), "proxy: keepalive hot: refreshed") {
		t.Errorf("log %q lacks the refreshed line", g.log.String())
	}
	if got := g.r.recentlyUsed(); len(got) != 1 {
		t.Errorf("recentlyUsed = %v, want the stale entry pruned", got)
	}
}

func TestKeepaliveIdleMachineMintsNothing(t *testing.T) {
	g := newKARig(t)
	g.r.keepaliveTick(t.Context())
	if got := g.called(); len(got) != 0 || g.log.String() != "" {
		t.Fatalf("idle tick refreshed %v, logged %q", got, g.log.String())
	}
}

func TestKeepaliveLogsVerdictNeverErrorText(t *testing.T) {
	g := newKARig(t)
	g.r.touch("hot", "idle", "stale")
	g.fn = func(_ context.Context, p *providers.Provider) (bool, error) {
		switch p.Name {
		case "hot":
			return false, errors.New("dapiSECRETTOKEN refused")
		case "idle":
			panic("dapiSECRETTOKEN in a panic")
		}
		return false, nil
	}
	g.r.keepaliveTick(t.Context())
	out := g.log.String()
	if strings.Contains(out, "SECRETTOKEN") {
		t.Fatalf("log leaked error text: %q", out)
	}
	for _, want := range []string{"proxy: keepalive hot: failed (transient)", "proxy: keepalive idle: failed (panic)"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q lacks %q", out, want)
		}
	}
	if got := g.called(); len(got) != 3 {
		t.Errorf("calls %v: a failure or panic must not stop the later profiles", got)
	}
}

func TestKeepaliveCallIsBoundedByTimeout(t *testing.T) {
	g := newKARig(t)
	g.r.ka.timeout = 20 * time.Millisecond
	g.r.touch("hot")
	g.fn = func(ctx context.Context, _ *providers.Provider) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}
	done := make(chan struct{})
	go func() { g.r.keepaliveTick(t.Context()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a hung refresh was not cut off by the call timeout")
	}
}

func TestKeepaliveJitterStaysInBounds(t *testing.T) {
	jitter := newKeepalive().jitter
	for range 2000 {
		if d := jitter(keepaliveJitter); d < 0 || d >= keepaliveJitter {
			t.Fatalf("jitter %s outside [0, %s)", d, keepaliveJitter)
		}
	}
}

// The loop waits interval+jitter between ticks, refreshes on each, and stop returns only once the goroutine is gone.
func TestKeepaliveLoopTicksAndStopsWithoutLeak(t *testing.T) {
	g := newKARig(t)
	g.r.touch("hot")
	var waits []time.Duration
	tick := make(chan struct{})
	exited := make(chan struct{})
	g.r.ka.jitter = func(max time.Duration) time.Duration { return max - 1 }
	g.r.ka.wait = func(ctx context.Context, d time.Duration) bool {
		g.mu.Lock()
		waits = append(waits, d)
		g.mu.Unlock()
		select {
		case <-tick:
			return true
		case <-ctx.Done():
			close(exited)
			return false
		}
	}
	stop := g.r.startKeepalive(t.Context())
	tick <- struct{}{} // first wait elapsed: one refresh pass
	tick <- struct{}{} // returns only after the first pass finished, so the call is recorded
	stop()
	select {
	case <-exited:
	default:
		t.Fatal("stop returned while the loop goroutine was still waiting")
	}
	if got := g.called(); len(got) < 1 || got[0] != "hot" {
		t.Fatalf("calls %v, want hot refreshed on a tick", got)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, d := range waits {
		if d < keepaliveEvery || d >= keepaliveEvery+keepaliveJitter {
			t.Errorf("wait %s outside [%s, %s)", d, keepaliveEvery, keepaliveEvery+keepaliveJitter)
		}
	}
}

func TestKeepaliveLoopStopsWhenParentContextEnds(t *testing.T) {
	g := newKARig(t)
	exited := make(chan struct{})
	g.r.ka.wait = func(ctx context.Context, _ time.Duration) bool { <-ctx.Done(); close(exited); return false }
	ctx, cancel := context.WithCancel(t.Context())
	stop := g.r.startKeepalive(ctx)
	cancel()
	stop()
	select {
	case <-exited:
	default:
		t.Fatal("the loop outlived its parent context")
	}
}

func TestRequestAndSessionScopeMarkProfilesUsed(t *testing.T) {
	g := newSessionRig(t)
	tok := g.mint(t, "dev")
	if got := g.r.recentlyUsed(); len(got) != 0 {
		t.Fatalf("minting a session marked %v used", got)
	}
	g.call(t, "/p/dev/v1/messages", `{}`, tok)
	if got := g.r.recentlyUsed(); len(got) != 1 || got[0] != "dev" {
		t.Fatalf("recentlyUsed = %v after a session request, want [dev]", got)
	}
	g.r.used = map[string]time.Time{}
	g.call(t, "/p/dev/v1/messages", `{}`, secret) // the router secret scopes nothing and routes nothing here
	if got := g.r.recentlyUsed(); len(got) != 0 {
		t.Errorf("an unscoped request that sent nothing marked %v", got)
	}
}

func TestSentRequestMarksItsProviderUsed(t *testing.T) {
	useGate(t)
	up := newUpstream(t, ok)
	var router *Router
	srv := setupRouter(t, `{"default":"up1","providers":{"databricks":{"up1":`+
		`{"protocol":"anthropic","base_url":"`+up.URL+`","auth":{"type":"oauth"},"models":{"opus":"m"}}}}}`,
		func(r *Router) { router = r })
	if resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m"}`, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want the 401 of an unsealed login", resp.StatusCode)
	}
	if got := router.recentlyUsed(); len(got) != 1 || got[0] != "up1" {
		t.Fatalf("recentlyUsed = %v, want [up1]", got)
	}
}

// The 401 for a dead login names the profile, the exact login command and whether the service principal was used.
func TestDeadLogin401NamesProfileCommandAndFallback(t *testing.T) {
	useGate(t)
	up := newUpstream(t, ok)
	srv := setupRouter(t, `{"default":"up1","providers":{"databricks":{"up1":`+
		`{"protocol":"anthropic","base_url":"`+up.URL+`","auth":{"type":"oauth"},"models":{"opus":"m"}}}}}`, nil)
	cmd := providers.LoginState{Profile: "up1"}.Command()
	cases := map[string]struct {
		hdr  map[string]string
		want string
	}{
		"auto":          {nil, "no service principal is paired, so there was no fallback"},
		"explicit user": {map[string]string{gateway.IdentityHeader: providers.IdentityUser}, "required the user identity"},
	}
	for name, c := range cases {
		b := body(t, post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m"}`, c.hdr))
		for _, want := range []string{"proxy: up1: authentication failed (needs_login); run `" + cmd + "`; ", c.want} {
			if !strings.Contains(b, want) {
				t.Errorf("%s: body %s lacks %q", name, b, want)
			}
		}
	}
}

func TestLoginTextSkipsRowsWithoutAUserLogin(t *testing.T) {
	ctx := providers.WithIdentity(context.Background(), providers.IdentitySP)
	oauth := &providers.Provider{Name: "p", Auth: providers.Auth{Type: providers.AuthOAuth}}
	if got := loginText(ctx, oauth, providers.VerdictNeedsLogin); got != "" {
		t.Errorf("sp identity: %q, want the generic text", got)
	}
	key := &providers.Provider{Name: "k", Auth: providers.Auth{Type: providers.AuthKey}}
	if got := loginText(context.Background(), key, providers.VerdictRejected); got != "" {
		t.Errorf("key row: %q, want the generic text", got)
	}
}
