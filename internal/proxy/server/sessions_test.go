package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/textutil"
)

type sessionRig struct {
	srv   *httptest.Server
	r     *Router
	mu    sync.Mutex
	now   time.Time
	start string // the fake process start time every pid reports
}

func newSessionRig(t *testing.T) *sessionRig {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	g := &sessionRig{now: time.Unix(1_700_000_000, 0), start: "100"}
	g.r = newRouter(secret)
	g.r.log = io.Discard
	g.r.now = func() time.Time { g.mu.Lock(); defer g.mu.Unlock(); return g.now }
	g.r.sessions.procStart = func(int) (string, error) { g.mu.Lock(); defer g.mu.Unlock(); return g.start, nil }
	g.srv = httptest.NewServer(g.r)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *sessionRig) advance(d time.Duration) { g.mu.Lock(); g.now = g.now.Add(d); g.mu.Unlock() }

func (g *sessionRig) setStart(s string) { g.mu.Lock(); g.start = s; g.mu.Unlock() }

func (g *sessionRig) call(t *testing.T, path, body, key string) (int, string) {
	t.Helper()
	code, b, _ := g.callH(t, path, body, key)
	return code, b
}

func (g *sessionRig) callH(t *testing.T, path, body, key string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, g.srv.URL+path, strings.NewReader(body))
	req.Header.Set("X-Api-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func (g *sessionRig) mint(t *testing.T, profile string) string {
	t.Helper()
	code, b := g.call(t, proxy.SessionPath, `{"profiles":["`+profile+`"],"pid":4242}`, secret)
	var out proxy.SessionResponse
	if code != 200 || json.Unmarshal([]byte(b), &out) != nil || out.Token == "" {
		t.Fatalf("mint = %d %s", code, b)
	}
	return out.Token
}

func TestSessionTokenScopeAndMintAuth(t *testing.T) {
	g := newSessionRig(t)
	tok := g.mint(t, "dev")
	for _, p := range []string{"/p/dev/v1/messages", "/p/dev/openai/v1/chat/completions", "/p/dev/responses/v1/responses"} {
		if code, _ := g.call(t, p, `{}`, tok); code == http.StatusUnauthorized {
			t.Errorf("%s: token rejected", p)
		}
	}
	if code, _ := g.call(t, "/p/other/v1/messages", `{}`, tok); code != http.StatusUnauthorized {
		t.Errorf("other profile = %d, want 401", code)
	}
	if code, _ := g.call(t, proxy.SessionPath, `{"profiles":["dev"],"pid":4242}`, tok); code != http.StatusUnauthorized {
		t.Errorf("mint with a session token = %d, want 401", code)
	}
}

func TestSessionTokenExpiresAtItsTTL(t *testing.T) {
	g := newSessionRig(t)
	tok := g.mint(t, "dev")
	g.advance(proxy.SessionTokenTTL - time.Minute)
	if c, _ := g.call(t, "/p/dev/v1/messages", `{}`, tok); c == http.StatusUnauthorized {
		t.Fatal("token refused inside its TTL")
	}
	g.advance(time.Minute)
	if c, _ := g.call(t, "/p/dev/v1/messages", `{}`, tok); c != http.StatusUnauthorized {
		t.Fatal("token accepted past its TTL")
	}
}

// A revoked.json an earlier build left behind is never read: it cannot refuse a live token.
func TestLeftoverRevokedFileIgnored(t *testing.T) {
	g := newSessionRig(t)
	tok := g.mint(t, "dev")
	writeLegacy(t, `{"schema":2,"sessions":[]}`) // creates the proxy state dir
	if err := os.WriteFile(filepath.Join(filepath.Dir(proxy.SessionsFile()), "revoked.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, _ := g.call(t, "/p/dev/v1/messages", `{}`, tok); c == http.StatusUnauthorized {
		t.Fatal("a leftover unreadable revoked.json refused a live token")
	}
}

func TestSessionTokenDiesWithReusedPid(t *testing.T) {
	g := newSessionRig(t)
	tok := g.mint(t, "dev")
	g.advance(sessionVerify)
	g.setStart("999") // the pid now belongs to another process
	if c, _ := g.call(t, "/p/dev/v1/messages", `{}`, tok); c != http.StatusUnauthorized {
		t.Fatalf("reused pid = %d, want 401", c)
	}
}

func TestSessionTokenKeepsLivenessCheckUnderFrequentRequests(t *testing.T) {
	g := newSessionRig(t)
	tok := g.mint(t, "dev")
	use := func() int { c, _ := g.call(t, "/p/dev/v1/messages", `{}`, tok); return c }
	for range 5 { // requests 1s apart never reach the 2s window on their own
		g.advance(time.Second)
		if use() == http.StatusUnauthorized {
			t.Fatal("live token rejected")
		}
	}
	g.setStart("999") // the process died and its pid was reused
	for range 3 {
		g.advance(time.Second)
		if use() == http.StatusUnauthorized {
			return
		}
	}
	t.Fatal("token outlived its process under frequent requests")
}

// A token minted by one daemon is valid on another of the same config.
func TestSessionTokenValidAcrossDaemons(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	a, b := newRouter(secret), newRouter(secret)
	ta, tb := mintOn(t, a), mintOn(t, b)
	for _, r := range []*Router{a, b, newRouter(secret)} {
		if !r.sessions.accept(ta, "dev") || !r.sessions.accept(tb, "dev") || r.sessions.accept(ta, "other") {
			t.Fatal("a daemon disagrees on a token minted by its peer")
		}
	}
}

func mintOn(t *testing.T, r *Router) string {
	t.Helper()
	tok, err := r.sessions.mint([]string{"dev"}, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestSessionTokenRefusedUnderAnotherSecret(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tok, err := newRouter(secret).sessions.mint([]string{"dev"}, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if newRouter("rotated-"+secret).sessions.accept(tok, "dev") {
		t.Fatal("a rotated router secret still accepts the old token")
	}
}

// A token whose claims were edited (a wider scope, another pid) fails its MAC.
func TestForgedSessionClaimsRefused(t *testing.T) {
	g := newSessionRig(t)
	tok := g.mint(t, "dev")
	payload, mac, _ := strings.Cut(strings.TrimPrefix(tok, proxy.SessionTokenPrefix), ".")
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	forged := strings.Replace(string(raw), `"p":["dev"]`, `"p":["dev","prod"]`, 1)
	tok2 := proxy.SessionTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(forged)) + "." + mac
	if c, body := g.call(t, "/p/prod/v1/messages", `{}`, tok2); c != http.StatusUnauthorized ||
		!strings.Contains(body, proxy.ErrSessionSignature.Error()) {
		t.Fatalf("forged scope = %d %q", c, body)
	}
}

func seenChecked(g *sessionRig, token string) time.Time {
	g.r.sessions.mu.Lock()
	defer g.r.sessions.mu.Unlock()
	if ses := g.r.sessions.seen[textutil.SHA256Hex([]byte(token))]; ses != nil {
		return ses.checked
	}
	return time.Time{}
}

func TestFailedIdentityProbeRejectsWithoutRenewingTrust(t *testing.T) {
	g := newSessionRig(t)
	token := g.mint(t, "dev")
	if c, _ := g.call(t, "/p/dev/v1/messages", `{}`, token); c == http.StatusUnauthorized {
		t.Fatal("fresh token refused")
	}
	before := seenChecked(g, token)
	var failed atomic.Bool
	failed.Store(true)
	g.r.sessions.procStart = func(int) (string, error) {
		if failed.Load() {
			return "", errors.New("process identity temporarily unavailable")
		}
		return "100", nil
	}
	g.advance(sessionVerify)
	for range 2 { // the second call sits inside a wrongly renewed window, if the failure had renewed one
		if code, _ := g.call(t, "/p/dev/v1/messages", `{}`, token); code != http.StatusUnauthorized {
			t.Fatalf("failed identity probe accepted request: status=%d, want 401", code)
		}
		g.advance(time.Second / 2)
	}
	if !seenChecked(g, token).Equal(before) {
		t.Fatal("failed identity probe renewed trust")
	}
	failed.Store(false)
	if code, _ := g.call(t, "/p/dev/v1/messages", `{}`, token); code == http.StatusUnauthorized {
		t.Fatal("session did not recover after a verified identity probe")
	}
	if !seenChecked(g, token).After(before) {
		t.Fatal("successful probe did not renew trust")
	}
}

// causeCase is one refused-session scenario: setup returns the request path and token, want is the named cause.
type causeCase struct {
	name  string
	setup func(g *sessionRig, t *testing.T) (path, tok string)
	want  string
}

func sessionCauseCases() []causeCase {
	const path = "/p/dev/v1/messages"
	legacy := proxy.SessionTokenPrefix + strings.Repeat("0", 64)
	return []causeCase{
		{"missing", func(g *sessionRig, t *testing.T) (string, string) { return path, "" }, "no token header sent"},
		{"legacy", func(g *sessionRig, t *testing.T) (string, string) { return path, legacy }, "not a signed session token"},
		{"expired", func(g *sessionRig, t *testing.T) (string, string) {
			tok := g.mint(t, "dev")
			g.advance(proxy.SessionTokenTTL)
			return path, tok
		}, whyExpired},
		{"exited", func(g *sessionRig, t *testing.T) (string, string) {
			tok := g.mint(t, "dev")
			g.advance(sessionVerify)
			g.setStart("999")
			return path, tok
		}, whyExited},
		{"out of scope", func(g *sessionRig, t *testing.T) (string, string) { return "/p/qa/v1/messages", g.mint(t, "dev") }, "dev only"},
	}
}

// checkRefusal asserts one refused call names its cause and the typed code, and leaks no token text.
func checkRefusal(t *testing.T, g *sessionRig, log *lockedBuf, path, tok, want string) {
	t.Helper()
	code, body, h := g.callH(t, path, `{}`, tok)
	if code != http.StatusUnauthorized || !strings.Contains(body, want) ||
		!strings.Contains(body, "proxy: missing or wrong client secret") || !strings.Contains(body, proxy.RefusalSession) ||
		h.Get(proxy.RefusalHeader) != proxy.RefusalSession {
		t.Fatalf("response = %d %q, want a typed 401 naming %q", code, body, want)
	}
	if !strings.Contains(log.String(), "refused 401 POST "+path+": proxy: missing or wrong client secret: ") ||
		!strings.Contains(log.String(), want) {
		t.Fatalf("log %q does not name %q", log.String(), want)
	}
	if tok != "" {
		checkNoLeak(t, tok, body, log.String())
	}
}

func checkNoLeak(t *testing.T, tok, body, log string) {
	t.Helper()
	for _, secretText := range []string{tok, tok[len(proxy.SessionTokenPrefix):], tok[:12], textutil.SHA256Hex([]byte(tok))} {
		if strings.Contains(body, secretText) || strings.Contains(log, secretText) {
			t.Fatalf("token text %q leaked into %q / %q", secretText, body, log)
		}
	}
}

// Every refused 401 names its cause in the response and the log line, and never carries the presented token.
func TestSessionRefusalNamesItsCause(t *testing.T) {
	for _, tc := range sessionCauseCases() {
		t.Run(tc.name, func(t *testing.T) {
			g := newSessionRig(t)
			var log lockedBuf
			g.r.log = &log
			path, tok := tc.setup(g, t)
			for range 2 { // a second refusal must still name the cause
				checkRefusal(t, g, &log, path, tok, tc.want)
			}
		})
	}
}

// A token the previous build stored works until its process exits; the bridge never writes the old file.
func TestLegacySessionTokenBridgedUntilItsProcessExits(t *testing.T) {
	g := newSessionRig(t)
	tok := proxy.SessionTokenPrefix + strings.Repeat("ab", 32)
	hash := textutil.SHA256Hex([]byte(tok))
	writeLegacy(t, `{"schema":2,"sessions":[{"hash":"`+hash+`","profiles":["dev"],"pid":4242,"start":"100",`+
		`"expires":"2023-11-20T00:00:00Z"}]}`)
	if c, _ := g.call(t, "/p/dev/v1/messages", `{}`, tok); c == http.StatusUnauthorized {
		t.Fatal("a stored old-format token was refused while its process lives")
	}
	if c, _ := g.call(t, "/p/other/v1/messages", `{}`, tok); c != http.StatusUnauthorized {
		t.Fatal("an old-format token was accepted outside its profiles")
	}
	g.advance(6 * 24 * time.Hour) // past the stored expiry: only the slide from the accepted use above keeps it
	if c, _ := g.call(t, "/p/dev/v1/messages", `{}`, tok); c == http.StatusUnauthorized {
		t.Fatal("an old-format token in use expired at its stored expiry")
	}
	g.advance(legacyTTL)
	if c, _ := g.call(t, "/p/dev/v1/messages", `{}`, tok); c != http.StatusUnauthorized {
		t.Fatal("an old-format token outlived its idle expiry")
	}
	g.advance(-legacyTTL)
	g.setStart("999")
	g.advance(sessionVerify)
	if c, _ := g.call(t, "/p/dev/v1/messages", `{}`, tok); c != http.StatusUnauthorized {
		t.Fatal("an old-format token outlived its process")
	}
	g.r.sessions.count()
	if raw, _ := os.ReadFile(proxy.SessionsFile()); !strings.Contains(string(raw), hash) {
		t.Fatal("the bridge rewrote sessions.json, which the previous build still writes without a lock")
	}
}

// A running daemon stops bridging a live old-format token once sessions.json is removed.
func TestLegacySessionTokenDroppedWhenSessionsFileRemoved(t *testing.T) {
	g := newSessionRig(t)
	tok := proxy.SessionTokenPrefix + strings.Repeat("cd", 32)
	hash := textutil.SHA256Hex([]byte(tok))
	writeLegacy(t, `{"schema":2,"sessions":[{"hash":"`+hash+`","profiles":["dev"],"pid":4242,"start":"100",`+
		`"expires":"2023-11-20T00:00:00Z"}]}`)
	if _, _, ok := g.r.sessions.legacyClaims(tok); !ok {
		t.Fatal("a stored old-format token was not live before sessions.json was removed")
	}
	if err := os.Remove(proxy.SessionsFile()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := g.r.sessions.legacyClaims(tok); ok {
		t.Fatal("a daemon kept bridging a token after sessions.json was removed")
	}
}

// A renewal one daemon made survives a takeover, never revives an idle token, and leaves with its dead holder.
func TestLegacyRenewalPersistsAcrossDaemons(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now, start := time.Unix(1_700_000_000, 0), "100"
	store := func() *sessionStore {
		s := newSessionStore(secret, func() time.Time { return now })
		s.procStart = func(int) (string, error) {
			if start == "" {
				return "", proc.ErrNoProcess
			}
			return start, nil
		}
		return s
	}
	tok := proxy.SessionTokenPrefix + strings.Repeat("cd", 32)
	hash := textutil.SHA256Hex([]byte(tok))
	writeLegacy(t, `{"schema":2,"sessions":[{"hash":"`+hash+`","profiles":["dev"],"pid":4242,"start":"100",`+
		`"expires":"`+now.Add(24*time.Hour).UTC().Format(time.RFC3339)+`"}]}`)
	if why := store().check(tok, "dev"); why != "" { // daemon 1 accepts and renews to now+7d
		t.Fatalf("daemon 1 refused a live old token: %s", why)
	}
	now = now.Add(6 * 24 * time.Hour) // past the saved expiry, inside the renewed one
	if why := store().check(tok, "dev"); why != "" {
		t.Fatalf("daemon 2 lost daemon 1's renewal: %s", why)
	}
	now = now.Add(legacyTTL) // idle past daemon 2's own renewal
	if why := store().check(tok, "dev"); why == "" {
		t.Fatal("a stale renewal revived an idle old token")
	}
	now = now.Add(-legacyTTL)
	start = ""
	s := store()
	s.pruneLegacy()
	if raw, err := os.ReadFile(proxy.LegacyRenewalsFile()); err != nil || strings.Contains(string(raw), hash) {
		t.Fatalf("a dead holder's renewal stayed in the file (err %v)", err)
	}
	if raw, _ := os.ReadFile(proxy.SessionsFile()); !strings.Contains(string(raw), hash) {
		t.Fatal("the bridge rewrote sessions.json")
	}
}

func writeLegacy(t *testing.T, v2 string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(proxy.SessionsFile()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(proxy.SessionsFile(), []byte(v2), 0o600); err != nil {
		t.Fatal(err)
	}
}
