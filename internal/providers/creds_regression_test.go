package providers

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/databricks/oauth"
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
	result := authFixWaitResult(t, leader) // The mint is still blocked on release: the leader must return first.
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
