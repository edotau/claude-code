package providers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/vault"
)

// sealSPAccess seals an SP access token valid for left into p's paired _ACCESS slot, bound to the harness secret.
func sealSPAccess(t *testing.T, p *Provider, left time.Duration) {
	t.Helper()
	b, _ := json.Marshal(clientAccess{Access: jwtExpIn(left, ""), Bind: clientBind(AuthHost(p), "sp-client", "sp-secret")})
	if err := vault.Store(SecretName(GroupDatabricks, p.Name, AuthM2M)+"_ACCESS", string(b)); err != nil {
		t.Fatal(err)
	}
}

func TestKeepaliveUserRefreshesAt14MinutesAndNotAt16(t *testing.T) {
	for _, c := range []struct {
		left time.Duration
		want bool
	}{{14 * time.Minute, true}, {16 * time.Minute, false}} {
		r := newLoginRig(t, "ka-user-"+c.left.String())
		r.mode.Store("ok")
		putAccess(t, r.p, jwtExpIn(c.left, "https://x"))
		holdRefreshLock(t, r.p) // a no-op keepalive must not even ask for the lock
		defer func(d time.Duration) { refreshLockWait = d }(refreshLockWait)
		refreshLockWait = 30 * time.Millisecond
		if !c.want {
			if got, err := Keepalive(context.Background(), r.p); err != nil || got || r.hits.Load() != 0 {
				t.Errorf("%v left: Keepalive = %v, %v (hits %d); want nothing due, no lock, no network", c.left, got, err, r.hits.Load())
			}
			continue
		}
		if _, err := Keepalive(context.Background(), r.p); !errors.Is(err, errRefreshLockTimeout) || r.hits.Load() != 0 {
			t.Errorf("%v left, lock held: err %v, hits %d; want the lock to be honored", c.left, err, r.hits.Load())
		}
	}
}

func TestKeepaliveUserRefreshRewritesTokenAndRecord(t *testing.T) {
	r := newLoginRig(t, "ka-user-write")
	r.mode.Store("ok")
	old := jwtExpIn(14*time.Minute, "https://x")
	putAccess(t, r.p, old)
	got, err := Keepalive(context.Background(), r.p)
	if err != nil || !got || r.hits.Load() != 1 {
		t.Fatalf("Keepalive = %v, %v (hits %d); want one refresh", got, err, r.hits.Load())
	}
	assertKeepaliveRewrote(t, r, old)
	if again, err := Keepalive(context.Background(), r.p); err != nil || again || r.hits.Load() != 1 {
		t.Errorf("second Keepalive = %v, %v (hits %d); want nothing due", again, err, r.hits.Load())
	}
}

// assertKeepaliveRewrote checks the refreshed token was sealed, recorded and cached in place of old.
func assertKeepaliveRewrote(t *testing.T, r *loginRig, old string) {
	t.Helper()
	if sealed := sealedAccessValue(r.p); sealed == old || sealed == "" {
		t.Error("the refreshed token was not sealed back")
	}
	if exp, ok := TokenExpiry(r.p.Name); !ok || time.Until(exp) < 50*time.Minute {
		t.Errorf("expiry record = %v, %v; want it rewritten to the new token's hour", exp, ok)
	}
	if v, ok := cacheGet(&cmdCache, r.p.Name); !ok || v != sealedAccessValue(r.p) {
		t.Error("the in-process cache does not hold the refreshed token")
	}
}

// A peer that refreshed while this call waited for the lock is adopted, never minted over.
func TestKeepaliveUserNeverDoubleMintsUnderTheLock(t *testing.T) {
	r := newLoginRig(t, "ka-user-peer")
	r.mode.Store("ok")
	putAccess(t, r.p, jwtExpIn(14*time.Minute, "https://x"))
	release := holdRefreshLock(t, r.p)
	type result struct {
		refreshed bool
		err       error
	}
	done := make(chan result, 1)
	go func() { ok, err := Keepalive(context.Background(), r.p); done <- result{ok, err} }()
	time.Sleep(100 * time.Millisecond)
	peer := jwtExpIn(time.Hour, "https://x")
	putAccess(t, r.p, peer)
	release()
	select {
	case res := <-done:
		if res.err != nil || res.refreshed || r.hits.Load() != 0 || sealedAccessValue(r.p) != peer {
			t.Fatalf("Keepalive = %+v (hits %d); want the peer's token adopted with no mint", res, r.hits.Load())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Keepalive never returned")
	}
}

func TestKeepaliveSkipsADeadLoginInsideItsWindowAndProbesAfter(t *testing.T) {
	r := newLoginRig(t, "ka-dead")
	if _, err := Credential(context.Background(), r.p); err == nil {
		t.Fatal("want the login dead")
	}
	assertKeepalive(t, r, "inside the window", false, 1, "it skipped")
	reopenWindow(t, r.p)
	assertKeepalive(t, r, "after the window, still dead", false, 2, "one probe and no refreshed")
	reopenWindow(t, r.p)
	r.mode.Store("ok") // renewed in another shell
	assertKeepalive(t, r, "after the window, healed", true, 3, "the probe to refresh")
	if _, dead := LoginStatus(r.p); dead {
		t.Error("the healed login stayed marked")
	}
}

// assertKeepalive runs one Keepalive and requires the refreshed result and the rig's total token-endpoint hits.
func assertKeepalive(t *testing.T, r *loginRig, label string, wantRefreshed bool, wantHits int32, want string) {
	t.Helper()
	got, err := Keepalive(context.Background(), r.p)
	if err != nil || got != wantRefreshed || r.hits.Load() != wantHits {
		t.Fatalf("%s: %v, %v (hits %d); want %s", label, got, err, r.hits.Load(), want)
	}
}

func TestKeepaliveSPRefreshesAt14MinutesAndRewritesItsRecord(t *testing.T) {
	p, m2m, _, _ := spHarness(t, IdentityAuto)
	mints := countingM2M(t, jwtExpIn(time.Hour, ""))
	sealSPAccess(t, p, 16*time.Minute)
	if got, err := Keepalive(context.Background(), p); err != nil || got || *mints != 0 {
		t.Fatalf("16 min left: %v, %v (mints %d); want nothing due", got, err, *mints)
	}
	sealSPAccess(t, p, 14*time.Minute)
	if got, err := Keepalive(context.Background(), p); err != nil || !got || *mints != 1 {
		t.Fatalf("14 min left: %v, %v (mints %d); want one SP mint", got, err, *mints)
	}
	if exp, ok := TokenExpiry(p.Name + spExpirySuffix); !ok || time.Until(exp) < 50*time.Minute {
		t.Errorf("SP expiry record = %v, %v; want it rewritten", exp, ok)
	}
	if *m2m != 0 {
		t.Errorf("the harness mint ran %d times; want the counting one only", *m2m)
	}
}

func TestKeepaliveFallsBackToTheExpiryRecordWhenNothingIsSealed(t *testing.T) {
	r := newLoginRig(t, "ka-record")
	r.mode.Store("ok")
	recordTokenExpiry(r.p.Name, jwtExpIn(16*time.Minute, ""), "u2m")
	if got, err := Keepalive(context.Background(), r.p); err != nil || got || r.hits.Load() != 0 {
		t.Fatalf("16 min on the record: %v, %v (hits %d); want nothing due", got, err, r.hits.Load())
	}
	recordTokenExpiry(r.p.Name, jwtExpIn(5*time.Minute, ""), "u2m")
	if got, err := Keepalive(context.Background(), r.p); err != nil || !got || r.hits.Load() != 1 {
		t.Fatalf("5 min on the record: %v, %v (hits %d); want a refresh", got, err, r.hits.Load())
	}
}

func TestKeepaliveM2MAndPAT(t *testing.T) {
	srv := newM2MRig(t)
	p := srv.p
	name := AuthSecretName(p)
	seal := func(left time.Duration) {
		b, _ := json.Marshal(clientAccess{Access: jwtExpIn(left, ""), Bind: clientBind(AuthHost(p), "sp-client", "sp-secret-xyz")})
		if err := vault.Store(name+"_ACCESS", string(b)); err != nil {
			t.Fatal(err)
		}
	}
	seal(16 * time.Minute)
	if got, err := Keepalive(context.Background(), p); err != nil || got || srv.posts.Load() != 0 {
		t.Fatalf("m2m 16 min left: %v, %v (posts %d); want nothing due", got, err, srv.posts.Load())
	}
	seal(14 * time.Minute)
	if got, err := Keepalive(context.Background(), p); err != nil || !got || srv.posts.Load() != 1 {
		t.Fatalf("m2m 14 min left: %v, %v (posts %d); want one mint", got, err, srv.posts.Load())
	}
	pat := mintRow(t, "ka-pat", AuthPAT, "", "http://127.0.0.1:1")
	sealFor(t, pat, "pat-secret")
	if got, err := Keepalive(context.Background(), pat); err != nil || got {
		t.Errorf("pat Keepalive = %v, %v; want nothing", got, err)
	}
}
