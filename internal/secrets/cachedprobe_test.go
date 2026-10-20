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
	meta := map[string]Meta{"KR": {Plane: "keyring"}}
	file := map[string]string{"F": "v"}

	t.Setenv("LOC_ENV_KR", "x")
	if src, sh := Locate("LOC_ENV_KR", map[string]Meta{"LOC_ENV_KR": {Plane: "keyring"}}, file); src != "env" || sh != "keyring" {
		t.Errorf("env+keyring: got (%q, %q), want (env, keyring)", src, sh)
	}
	if src, sh := Locate("KR", meta, file); src != "keyring" || sh != "" {
		t.Errorf("keyring only: got (%q, %q), want (keyring, \"\")", src, sh)
	}
	if src, sh := Locate("F", meta, file); src != "file" || sh != "" {
		t.Errorf("file only: got (%q, %q), want (file, \"\")", src, sh)
	}
	if src, sh := Locate("NOWHERE", meta, file); src != "" || sh != "" {
		t.Errorf("none: got (%q, %q), want (\"\", \"\")", src, sh)
	}
}
