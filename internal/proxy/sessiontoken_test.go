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

func TestLegacySessionShape(t *testing.T) {
	tok, _ := SignSessionToken("s1", SessionClaims{Profiles: []string{"dev"}, PID: 42, Start: "7"}, time.Now())
	if !LegacySessionShape("cct_"+strings.Repeat("0", 64)) || LegacySessionShape(tok) {
		t.Error("only a cct_ + 64 hex token has the legacy shape")
	}
}

// One MAC spelling (one hash per token), a one-year life, and one config dir per token.
func TestSessionTokenOneSpellingOneYearOneConfig(t *testing.T) {
	const oneYear = 365 * 24 * time.Hour // a passthrough header is never renewed
	tok, err := SignSessionToken("s1", SessionClaims{Profiles: []string{"dev"}, PID: 42, Start: "7"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSessionToken("s1", tok[:len(tok)-4]+"\r\n"+tok[len(tok)-4:]); err == nil {
		t.Error("a second spelling of the same MAC (CR/LF inside) parsed")
	}
	if SessionTokenTTL != oneYear {
		t.Errorf("SessionTokenTTL = %v, want %v", SessionTokenTTL, oneYear)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if _, err := ParseSessionToken("s1", tok); !errors.Is(err, ErrSessionSignature) {
		t.Errorf("another config dir with the same secret = %v, want ErrSessionSignature", err)
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

func TestSessionTokenFenceIsSignedAndEnforced(t *testing.T) {
	now := time.Now()
	start := func(int) (string, error) { return "7", nil }
	c := SessionClaims{Profiles: []string{"dev"}, PID: 42, Start: "7", Fence: []string{"dev", "own"}}
	tok, err := SignSessionToken("s1", c, now)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := PeekSessionClaims(tok); !got.FenceAllows("own") || got.FenceAllows("qa") {
		t.Errorf("signed fence = %v", got.Fence)
	}
	if CheckSessionToken("s1", tok, "dev", now, start) != nil {
		t.Error("in-fence profile refused")
	}
	c.Profiles = []string{"qa"}
	if _, err := SignSessionToken("s1", c, now); err == nil {
		t.Error("a token wider than its fence was signed")
	}
	if _, err := SignSessionToken("s1", SessionClaims{Profiles: []string{"dev"}, PID: 42, Start: "7", Open: true, Fence: []string{"dev"}}, now); err == nil {
		t.Error("open and fenced together was signed")
	}
}
