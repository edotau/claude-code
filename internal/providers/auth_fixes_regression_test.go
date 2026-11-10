package providers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// authFixBody returns a complete catalog and an error in the same Read.
type authFixBody struct {
	err    error
	closed bool
}

func (b *authFixBody) Read(p []byte) (int, error) {
	return copy(p, `{"data":[{"id":"complete-model"}]}`), b.err
}

func (b *authFixBody) Close() error { b.closed = true; return nil }

type authFixTransport struct{ body io.ReadCloser }

func (r authFixTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{}, Body: r.body, Request: req}, nil
}

func TestLiveModelEntriesRejectsReadErrorWithValidJSON(t *testing.T) {
	vaulttest.Use(t)
	readErr := errors.New("catalog stream interrupted")
	body := &authFixBody{err: readErr}
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = authFixTransport{body: body}
	t.Cleanup(func() { http.DefaultClient.Transport = old })
	p := &Provider{Name: "read-error", Protocol: ProtocolOpenAI, BaseURL: "https://catalog.invalid", Auth: Auth{Type: AuthNone}}
	entries, err := liveModelEntries(context.Background(), p)
	if !errors.Is(err, readErr) || len(entries) != 0 {
		t.Fatalf("catalog accepted an errored body: entries=%d, err=%v", len(entries), err)
	}
	if !body.closed {
		t.Fatal("catalog body was not closed")
	}
}

func TestResolveProfileStoredCredentialWithoutEnv(t *testing.T) {
	vaulttest.Use(t)
	for _, typ := range []string{AuthPAT, AuthOAuth, AuthM2M, AuthKey} {
		t.Run(typ, func(t *testing.T) {
			p := &Provider{Name: "stored-" + typ, Provider: GroupDatabricks, Auth: Auth{Type: typ}}
			name := SecretName(p.Provider, p.Name, typ)
			t.Setenv(name, "")
			if got := Resolve(p); got.Source != "none" {
				t.Fatalf("unseeded source = %q, want none", got.Source)
			}
			vaulttest.Secret(t, name, "profile-credential")
			if got := Resolve(p); got.Source != "vault" || got.Note != "" {
				t.Fatalf("stored profile status = %+v, want vault without a warning", got)
			}
		})
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

func TestSealLoginWrongIssuerPreservesStoredTokens(t *testing.T) {
	vaulttest.Use(t)
	p := &Provider{Name: "login-issuer", Provider: GroupDatabricks, Preset: PresetDatabricks,
		Host: "https://expected.example.com", Auth: Auth{Type: AuthOAuth}}
	const previous = `{"refresh":"previous-refresh","access":"previous-access"}`
	vaulttest.Secret(t, AuthSecretName(p), previous)
	cmdCache.Delete(p.Name)
	t.Cleanup(func() { cmdCache.Delete(p.Name) })
	_, err := sealLogin(p, oauth.Token{Refresh: "wrong-refresh",
		Access: jwtExpIn(time.Hour, "https://wrong.example.com/oidc"), Expiry: time.Now().Add(time.Hour)})
	if _, ok := errors.AsType[*HostMismatchError](err); !ok {
		t.Fatalf("sealLogin error = %v, want HostMismatchError", err)
	}
	stored, found, readErr := vault.ReadSecret(AuthSecretName(p))
	if readErr != nil || !found || stored != previous {
		t.Fatalf("wrong issuer replaced stored tokens: found=%t unchanged=%t err=%v", found, stored == previous, readErr)
	}
	if _, found := cmdCache.Load(p.Name); found {
		t.Fatal("wrong issuer populated the command cache")
	}
}

// The legacy sweep deletes an unreadable hash slot without decrypting it, marks the vault and keeps other names.
func TestDropLegacyAccessSweepsUnreadableSlotOnce(t *testing.T) {
	vaulttest.Use(t)
	legacySwept.Store(false)
	t.Cleanup(func() { legacySwept.Store(false) })
	const slot, keep = "DATABRICKS_M2M_0123456789ABCDEF_ACCESS", "DATABRICKS_DEV_M2M_ACCESS"
	vaulttest.Put(t, vault.Secrets, slot, "{malformed-record")
	if err := vault.Store(keep, "kept"); err != nil {
		t.Fatal(err)
	}
	dropLegacyAccess()
	names, err := vault.NamesErr()
	if err != nil || slices.Contains(names, slot) || !slices.Contains(names, keep) {
		t.Fatalf("sweep: names = %v, err = %v", names, err)
	}
	legacySwept.Store(false) // a new process: the vault marker alone must stop a second sweep
	vaulttest.Put(t, vault.Secrets, slot, "{malformed-record")
	dropLegacyAccess()
	if names, _ := vault.NamesErr(); !slices.Contains(names, slot) {
		t.Fatal("a marked vault was swept again")
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
