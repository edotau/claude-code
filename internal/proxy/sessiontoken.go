package proxy

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// SessionTokenPrefix opens every signed session token: cct_<claims>.<mac>, both base64url.
const SessionTokenPrefix = "cct_"

// SessionTokenTTL outlasts any session (a passthrough header is never renewed); the bound process is the real limit.
const SessionTokenTTL = 365 * 24 * time.Hour

// RefusalHeader names why the proxy itself refused a request; RefusalSession is its one session-token value.
const (
	RefusalHeader  = "X-Claude-Code-Proxy-Refusal"
	RefusalSession = "session-refused"
)

// sessionKeyLabel separates the token MAC key from every other use of the router secret.
const sessionKeyLabel = "claude-code proxy session token v1"

// maxSessionToken caps what is decoded from an untrusted header.
const maxSessionToken = 4096

// SessionClaims is a token's signed payload: plain base64, so it carries nothing secret.
type SessionClaims struct {
	Profiles []string `json:"p"`
	Failover string   `json:"f,omitempty"`
	PID      int      `json:"pid"`
	Start    string   `json:"s"`
	Issued   int64    `json:"iat"`
	Expires  int64    `json:"exp"`
	Nonce    string   `json:"n"`
}

// Token-shape errors; ParseSessionToken wraps each so the refusal can name it.
var (
	ErrSessionMalformed = errors.New("not a signed session token")
	ErrSessionSignature = errors.New("session token was not signed with this config's router secret")
)

// keyMemo holds the last derived key: every request parses its token, and the secret rarely changes.
var keyMemo struct {
	sync.Mutex
	secret, config string
	key            []byte
}

// sessionKey binds the MAC key to this config dir too, so a copied vault secret never shares tokens across configs.
func sessionKey(secret string) ([]byte, error) {
	config := ConfigID()
	keyMemo.Lock()
	defer keyMemo.Unlock()
	if keyMemo.key != nil && keyMemo.secret == secret && keyMemo.config == config {
		return keyMemo.key, nil
	}
	key, err := hkdf.Key(sha256.New, []byte(secret), nil, sessionKeyLabel+" "+config, sha256.Size)
	if err != nil {
		return nil, err
	}
	keyMemo.secret, keyMemo.config, keyMemo.key = secret, config, key
	return key, nil
}

// SignSessionToken issues a token for c under secret, stamping issue time, expiry and a nonce.
func SignSessionToken(secret string, c SessionClaims, now time.Time) (string, error) {
	n := make([]byte, 8)
	if _, err := rand.Read(n); err != nil {
		return "", err
	}
	c.Profiles = slices.Compact(slices.Sorted(slices.Values(c.Profiles)))
	c.Issued, c.Expires, c.Nonce = now.Unix(), now.Add(SessionTokenTTL).Unix(), hex.EncodeToString(n)
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac, err := sessionMAC(secret, payload)
	if err != nil {
		return "", err
	}
	tok := SessionTokenPrefix + payload + "." + base64.RawURLEncoding.EncodeToString(mac)
	if len(tok) > maxSessionToken {
		return "", fmt.Errorf("session token would be %d bytes (max %d): too many profiles", len(tok), maxSessionToken)
	}
	return tok, nil
}

func sessionMAC(secret, payload string) ([]byte, error) {
	key, err := sessionKey(secret)
	if err != nil {
		return nil, err
	}
	h := hmac.New(sha256.New, key)
	h.Write([]byte(payload))
	return h.Sum(nil), nil
}

// ParseSessionToken checks tok's MAC under secret and returns its claims; expiry and liveness are the caller's.
func ParseSessionToken(secret, tok string) (SessionClaims, error) {
	var c SessionClaims
	payload, got, ok := splitSessionToken(tok)
	if !ok {
		return c, ErrSessionMalformed
	}
	want, err := sessionMAC(secret, payload)
	if err != nil {
		return c, err
	}
	if !hmac.Equal(got, want) {
		return c, ErrSessionSignature
	}
	if c, ok = decodeClaims(payload); !ok || c.PID <= 0 || c.Start == "" || len(c.Profiles) == 0 {
		return SessionClaims{}, ErrSessionMalformed
	}
	return c, nil
}

// splitSessionToken returns tok's claims payload and decoded MAC; ok is false for any other shape.
func splitSessionToken(tok string) (payload string, mac []byte, ok bool) {
	body, pf := strings.CutPrefix(tok, SessionTokenPrefix)
	payload, sig, found := strings.Cut(body, ".")
	if !pf || !found || len(tok) > maxSessionToken {
		return "", nil, false
	}
	mac, err := base64.RawURLEncoding.Strict().DecodeString(sig)
	// One spelling per MAC, so a revoked hash holds: Strict still skips CR/LF, so re-encode and compare.
	return payload, mac, err == nil && base64.RawURLEncoding.EncodeToString(mac) == sig
}

func decodeClaims(payload string) (SessionClaims, bool) {
	var c SessionClaims
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || json.Unmarshal(raw, &c) != nil {
		return SessionClaims{}, false
	}
	return c, true
}

// PeekSessionClaims decodes tok's claims without checking the MAC: for routing a re-mint, never for auth.
func PeekSessionClaims(tok string) (SessionClaims, bool) {
	payload, _, ok := splitSessionToken(tok)
	if !ok {
		return SessionClaims{}, false
	}
	return decodeClaims(payload)
}

// SessionClockSkew is how far a token's issue time may lead the clock: beyond it the clock stepped back, re-mint.
const SessionClockSkew = 5 * time.Minute

// ErrSessionExpired covers both ends of a token's window: past its expiry, or issued ahead of the clock.
var ErrSessionExpired = errors.New("session token expired")

// SessionWindowErr is ErrSessionExpired when now lies outside c's issue-to-expiry window.
func SessionWindowErr(c SessionClaims, now time.Time) error {
	if now.Unix() >= c.Expires || now.Add(SessionClockSkew).Unix() < c.Issued {
		return ErrSessionExpired
	}
	return nil
}

// CheckSessionToken reports why tok cannot serve profile ("" = any) now: bad MAC, window, scope, or a gone process.
func CheckSessionToken(secret, tok, profile string, now time.Time, start func(int) (string, error)) error {
	c, err := ParseSessionToken(secret, tok)
	if err != nil {
		return err
	}
	if err := SessionWindowErr(c, now); err != nil {
		return err
	}
	if profile != "" && !slices.Contains(c.Profiles, profile) {
		return fmt.Errorf("session token is not scoped to %s", profile)
	}
	if got, err := start(c.PID); err != nil || got != c.Start {
		return fmt.Errorf("the process the session token was bound to has exited (pid %d)", c.PID)
	}
	return nil
}
