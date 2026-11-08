package providers

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/databricks/oauth"
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
