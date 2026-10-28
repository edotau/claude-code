package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sealedKinds are the sealed authTypes whose mint goes through the OIDC token endpoint.
var sealedKinds = []struct{ kind, clientID, secret string }{
	{AuthOAuth, "", "refresh-1"},
	{AuthM2M, "sp-client", "sp-secret-xyz"},
}

func TestSealedAuthHerdMintsOnce(t *testing.T) {
	for _, k := range sealedKinds {
		t.Run(k.kind, func(t *testing.T) {
			var posts atomic.Int32
			srv := oidcServer(t, func(*http.Request, url.Values) (int, map[string]any) {
				posts.Add(1)
				time.Sleep(150 * time.Millisecond) // keep the flight open while the herd arrives
				return 200, map[string]any{"access_token": "herd-token", "expires_in": 3600}
			})
			p := mintRow(t, "sf-herd-"+k.kind, k.kind, k.clientID, srv.URL)
			sealFor(t, p, k.secret)
			vals, errs := herd(p, 20)
			for i := range vals {
				if errs[i] != nil || vals[i] != "herd-token" {
					t.Fatalf("caller %d: %d-byte token, %v", i, len(vals[i]), errs[i])
				}
			}
			if n := posts.Load(); n != 1 {
				t.Fatalf("token endpoint hit %d times, want 1", n)
			}
		})
	}
}

func TestSealedAuthRefreshDuringInflightMintRunsForcedMint(t *testing.T) {
	for _, k := range sealedKinds {
		t.Run(k.kind, func(t *testing.T) {
			var posts atomic.Int32
			gate := make(chan struct{})
			srv := oidcServer(t, func(*http.Request, url.Values) (int, map[string]any) {
				n := posts.Add(1)
				if n == 1 {
					<-gate // the non-forced mint stays in flight until released
				}
				return 200, map[string]any{"access_token": fmt.Sprintf("tok-%d", n), "expires_in": 3600}
			})
			p := mintRow(t, "sf-refresh-"+k.kind, k.kind, k.clientID, srv.URL)
			sealFor(t, p, k.secret)
			var wg sync.WaitGroup
			wg.Add(1)
			go func() { defer wg.Done(); _, _ = Credential(context.Background(), p) }()
			deadline := time.Now().Add(5 * time.Second)
			for posts.Load() == 0 {
				if time.Now().After(deadline) {
					close(gate)
					t.Fatal("non-forced mint never reached the endpoint")
				}
				time.Sleep(10 * time.Millisecond)
			}
			time.AfterFunc(150*time.Millisecond, func() { close(gate) }) // Refresh is queued behind the flight by then
			got, err := Refresh(context.Background(), p)
			wg.Wait()
			if err != nil || got != "tok-2" {
				t.Fatalf("Refresh = %d-byte token, %v; want the forced mint tok-2, not the in-flight result", len(got), err)
			}
			if n := posts.Load(); n != 2 {
				t.Fatalf("token endpoint hit %d times, want 2 (one non-forced, one forced)", n)
			}
		})
	}
}
