package proxy

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSessionTokenSignParseAndRefusals(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tok, err := SignSessionToken("s1", SessionClaims{Profiles: []string{"qa", "dev", "qa"}, PID: 42, Start: "7"}, now)
	if err != nil || !strings.HasPrefix(tok, SessionTokenPrefix) {
		t.Fatalf("sign = %v", err)
	}
	c, err := ParseSessionToken("s1", tok)
	if err != nil || strings.Join(c.Profiles, ",") != "dev,qa" || c.PID != 42 || c.Expires != now.Add(SessionTokenTTL).Unix() {
		t.Fatalf("parse = %+v, %v", c, err)
	}
	if _, err := ParseSessionToken("s2", tok); !errors.Is(err, ErrSessionSignature) {
		t.Errorf("other secret = %v, want ErrSessionSignature", err)
	}
	if _, err := ParseSessionToken("s1", tok[:len(tok)-2]+"AA"); err == nil {
		t.Error("an edited MAC parsed")
	}
	if _, err := ParseSessionToken("s1", "cct_"+strings.Repeat("0", 64)); !errors.Is(err, ErrSessionMalformed) {
		t.Errorf("legacy token = %v, want ErrSessionMalformed", err)
	}
}

func TestCheckSessionTokenExpiryAndPid(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tok, err := SignSessionToken("s1", SessionClaims{Profiles: []string{"dev"}, PID: 42, Start: "7"}, now)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		profile string
		at      time.Time
		start   string
		ok      bool
	}{
		"live": {"dev", now.Add(time.Hour), "7", true}, "expired": {"", now.Add(SessionTokenTTL), "7", false},
		"clock stepped back": {"", now.Add(-SessionClockSkew - time.Minute), "7", false},
		"recycled pid":       {"", now, "8", false}, "other route": {"prod", now, "7", false},
	} {
		start := func(int) (string, error) { return c.start, nil }
		if err := CheckSessionToken("s1", tok, c.profile, c.at, start); (err == nil) != c.ok {
			t.Errorf("%s: err %v, want ok=%v", name, err, c.ok)
		}
	}
	if c, ok := PeekSessionClaims(tok); !ok || c.Expires != now.Add(SessionTokenTTL).Unix() {
		t.Errorf("PeekSessionClaims = %+v, %v", c, ok)
	}
}
