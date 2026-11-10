package serving

import (
	"encoding/base64"
	"fmt"
	"testing"
	"time"
)

// unsignedJWT is a JWT-shaped key whose only meaningful claim is sub.
func unsignedJWT(sub string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(fmt.Sprintf(`{"sub":%q}`, sub))) + ".sig"
}

// A fresh sidecar for the same credential identity serves the cache with no /Me call; refresh always validates.
func TestServingDiscoverySidecarSkipsIdentityCall(t *testing.T) {
	p, f := discoveryEnv(t)
	t.Setenv("DISCOVERY_TEST_TOKEN", unsignedJWT("user-a"))
	if err := Save(p, discoverForTest(t, p, false)); err != nil {
		t.Fatal(err)
	}
	if hit := discoverForTest(t, p, false); !hit.CacheHit || f.me.Load() != 1 || f.list.Load() != 1 {
		t.Fatalf("same identity: hit=%v Me=%d listing=%d", hit.CacheHit, f.me.Load(), f.list.Load())
	}
	if discoverForTest(t, p, true); f.me.Load() != 2 {
		t.Fatalf("refresh skipped identity validation: Me=%d", f.me.Load())
	}
}

// A cache written for one identity is never served to another, even with a fresh sidecar on the same profile.
func TestServingDiscoverySidecarIsIdentityScoped(t *testing.T) {
	p, f := discoveryEnv(t)
	t.Setenv("DISCOVERY_TEST_TOKEN", unsignedJWT("user-a"))
	a := discoverForTest(t, p, false)
	if err := Save(p, a); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DISCOVERY_TEST_TOKEN", unsignedJWT("user-b"))
	f.identity.Store(`{"id":"subject-two","active":true}`)
	b := discoverForTest(t, p, false)
	if b.CacheHit || b.Scope.SubjectID != "subject-two" || f.me.Load() != 2 || f.list.Load() != 2 {
		t.Fatalf("other identity: hit=%v subject=%q Me=%d listing=%d", b.CacheHit, b.Scope.SubjectID, f.me.Load(), f.list.Load())
	}
}

// An expired sidecar revalidates.
func TestServingDiscoverySidecarExpires(t *testing.T) {
	p, f := discoveryEnv(t)
	t.Setenv("DISCOVERY_TEST_TOKEN", unsignedJWT("user-a"))
	if err := Save(p, discoverForTest(t, p, false)); err != nil {
		t.Fatal(err)
	}
	name, err := validatedKey(p)
	if err != nil {
		t.Fatal(err)
	}
	rememberValidated(p, name, validated{Subject: "subject-one", TokenSub: "user-a", At: time.Now().Add(-identityTTL - time.Minute)})
	if hit := discoverForTest(t, p, false); !hit.CacheHit || f.me.Load() != 2 {
		t.Fatalf("expired sidecar: hit=%v Me=%d", hit.CacheHit, f.me.Load())
	}
}
