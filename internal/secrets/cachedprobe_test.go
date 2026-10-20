package secrets

import (
	"testing"
	"time"
)

func TestCachedProbe(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ttl := time.Hour

	cases := []struct {
		name       string
		m          Meta
		wantStatus string
		wantAge    time.Duration
		wantOK     bool
	}{
		{"verified within ttl", Meta{VerifyStatus: "verified", VerifiedAt: now.Add(-10 * time.Minute)}, "verified", 10 * time.Minute, true},
		{"rejected within ttl", Meta{VerifyStatus: "rejected", VerifiedAt: now.Add(-10 * time.Minute)}, "rejected", 10 * time.Minute, true},
		{"unreachable", Meta{VerifyStatus: "unreachable", VerifiedAt: now.Add(-10 * time.Minute)}, "", 10 * time.Minute, false},
		{"verified older than ttl", Meta{VerifyStatus: "verified", VerifiedAt: now.Add(-2 * time.Hour)}, "", 2 * time.Hour, false},
		{"verified at ttl", Meta{VerifyStatus: "verified", VerifiedAt: now.Add(-ttl)}, "", ttl, false},
		{"zero VerifiedAt", Meta{}, "", 0, false},
	}
	for _, c := range cases {
		status, age, ok := CachedProbe(c.m, ttl, now)
		if status != c.wantStatus || age != c.wantAge || ok != c.wantOK {
			t.Errorf("%s: CachedProbe = (%q, %v, %v), want (%q, %v, %v)",
				c.name, status, age, ok, c.wantStatus, c.wantAge, c.wantOK)
		}
	}
}

func TestLocate(t *testing.T) {
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	for _, tc := range []struct {
		name, env, source, shadowed string
		stored                      bool
	}{
		{"env over vault", "x", "env", "vault", true},
		{"vault only", "", "vault", "", true},
		{"env only", "x", "env", "", false},
		{"none", "", "", "", false},
		{"blank env over vault", " \n", "vault", "", true},
		{"blank env", " \n", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LOCATE_TEST", tc.env)
			meta := map[string]Meta{}
			if tc.stored {
				meta["LOCATE_TEST"] = Meta{}
			}
			if src, sh := Locate("LOCATE_TEST", meta); src != tc.source || sh != tc.shadowed {
				t.Errorf("Locate = (%q, %q), want (%q, %q)", src, sh, tc.source, tc.shadowed)
			}
		})
	}
}
