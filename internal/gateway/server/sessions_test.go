package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/gateway"
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
	req, _ := http.NewRequest(http.MethodPost, g.srv.URL+path, strings.NewReader(body))
	req.Header.Set("X-Api-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (g *sessionRig) mint(t *testing.T, profile string) string {
	t.Helper()
	code, b := g.call(t, gateway.SessionPath, `{"profiles":["`+profile+`"],"pid":4242}`, secret)
	var out gateway.SessionResponse
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
	if code, _ := g.call(t, gateway.SessionPath, `{"profiles":["dev"],"pid":4242}`, tok); code != http.StatusUnauthorized {
		t.Errorf("mint with a session token = %d, want 401", code)
	}
	if code, _ := g.call(t, gateway.SessionRevokePath, `{"token":"`+tok+`"}`, tok); code != http.StatusUnauthorized {
		t.Errorf("revoke with a session token = %d, want 401", code)
	}
	if code, _ := g.call(t, "/p/dev/v1/messages", `{}`, tok); code == http.StatusUnauthorized {
		t.Error("token died after a rejected revoke")
	}
}

func TestSessionTokenIdleExpiryAndSlidingRenewal(t *testing.T) {
	g := newSessionRig(t)
	tok := g.mint(t, "dev")
	use := func() int { c, _ := g.call(t, "/p/dev/v1/messages", `{}`, tok); return c }
	for range 3 { // 3 x 4d exceeds the 7d TTL only if use does not renew it
		g.advance(4 * 24 * time.Hour)
		if use() == http.StatusUnauthorized {
			t.Fatal("renewed token expired")
		}
	}
	g.advance(sessionTTL)
	if use() != http.StatusUnauthorized {
		t.Fatal("idle token still accepted past the TTL")
	}
}

func TestSessionTokenDiesWithReusedPidAndOnRevoke(t *testing.T) {
	g := newSessionRig(t)
	tok := g.mint(t, "dev")
	g.advance(sessionVerify)
	g.setStart("999") // the pid now belongs to another process
	if c, _ := g.call(t, "/p/dev/v1/messages", `{}`, tok); c != http.StatusUnauthorized {
		t.Fatalf("reused pid = %d, want 401", c)
	}
	g.setStart("100")
	tok = g.mint(t, "dev")
	if c, _ := g.call(t, gateway.SessionRevokePath, `{"token":"`+tok+`"}`, secret); c != http.StatusNoContent {
		t.Fatalf("revoke = %d", c)
	}
	if c, _ := g.call(t, "/p/dev/v1/messages", `{}`, tok); c != http.StatusUnauthorized {
		t.Fatalf("revoked token = %d, want 401", c)
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

func TestSessionMintPrunesDeadProcesses(t *testing.T) {
	g := newSessionRig(t)
	for range sessionMax {
		g.mint(t, "dev")
	}
	g.setStart("999") // every minted process is gone
	g.advance(time.Second)
	g.mint(t, "dev")
}

func TestSessionTokenSurvivesRouterRestart(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	first := newRouter(secret)
	token, err := first.sessions.mint([]string{"dev"}, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}

	restarted := newRouter(secret)
	if !restarted.sessions.accept(token, "dev") {
		t.Fatal("restarted router rejected a live session token")
	}
	if restarted.sessions.accept(token, "other") {
		t.Fatal("restarted router accepted a session token for another profile")
	}
}

func TestRevokedSessionTokenStaysRevokedAfterRestart(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	first := newRouter(secret)
	token, err := first.sessions.mint([]string{"dev"}, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}

	restarted := newRouter(secret)
	if !restarted.sessions.accept(token, "dev") {
		t.Fatal("restarted router rejected the token before revocation")
	}
	restarted.sessions.revoke(token)
	if newRouter(secret).sessions.accept(token, "dev") {
		t.Fatal("revoked session token returned after another restart")
	}
}

func TestSessionTokenPersistenceIsPrivateAndHashed(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	router := newRouter(secret)
	token, err := router.sessions.mint([]string{"dev"}, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}

	path := sessionsFile()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("sessions.json mode = %04o, want 0600", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(token)) {
		t.Fatal("sessions.json contains the plaintext session token")
	}
	hash := sha256.Sum256([]byte(token))
	if !bytes.Contains(data, []byte(hex.EncodeToString(hash[:]))) {
		t.Fatal("sessions.json does not contain the session token's SHA-256 hash")
	}
}

func sessionSnapshot(t *testing.T, g *sessionRig, token string) session {
	t.Helper()
	g.r.sessions.mu.Lock()
	defer g.r.sessions.mu.Unlock()
	ses := g.r.sessions.m[textutil.SHA256Hex([]byte(token))]
	if ses == nil {
		t.Fatal("session unexpectedly removed")
	}
	return *ses
}

func TestFailedIdentityProbeRejectsWithoutRenewingTrust(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	g := newSessionRig(t)
	token := g.mint(t, "dev")
	before := sessionSnapshot(t, g, token)
	var failed atomic.Bool
	failed.Store(true)
	g.r.sessions.procStart = func(int) (string, error) {
		if failed.Load() {
			return "", errors.New("process identity temporarily unavailable")
		}
		return "100", nil
	}
	g.advance(sessionVerify)
	if code, _ := g.call(t, "/p/dev/v1/messages", `{}`, token); code != http.StatusUnauthorized {
		t.Errorf("failed identity probe accepted request: status=%d, want 401", code)
	}
	if after := sessionSnapshot(t, g, token); !reflect.DeepEqual(after, before) {
		t.Errorf("failed identity probe renewed session: before=%+v after=%+v", before, after)
	}
	g.advance(time.Second) // inside a wrongly renewed trust window: failure must still reject.
	if code, _ := g.call(t, "/p/dev/v1/messages", `{}`, token); code != http.StatusUnauthorized {
		t.Errorf("failed identity probe gained cached trust: status=%d, want 401", code)
	}
	failed.Store(false)
	if code, _ := g.call(t, "/p/dev/v1/messages", `{}`, token); code == http.StatusUnauthorized {
		t.Fatal("session did not recover after a verified identity probe")
	}
	after := sessionSnapshot(t, g, token)
	elapsed := sessionVerify + time.Second
	if !after.checked.Equal(before.checked.Add(elapsed)) || !after.expires.Equal(before.expires.Add(elapsed)) {
		t.Fatalf("successful probe did not renew trust and expiry: before=%+v after=%+v", before, after)
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
	unknown := sessionTokenPf + strings.Repeat("0", 64)
	return []causeCase{
		{"missing", func(g *sessionRig, t *testing.T) (string, string) { return path, "" }, "no token header sent"},
		{"unknown", func(g *sessionRig, t *testing.T) (string, string) { return path, unknown }, "never minted here"},
		{"expired", func(g *sessionRig, t *testing.T) (string, string) {
			tok := g.mint(t, "dev")
			g.advance(sessionTTL)
			return path, tok
		}, whyExpired},
		{"revoked", func(g *sessionRig, t *testing.T) (string, string) {
			tok := g.mint(t, "dev")
			if c, _ := g.call(t, gateway.SessionRevokePath, `{"token":"`+tok+`"}`, secret); c != http.StatusNoContent {
				t.Fatalf("revoke = %d", c)
			}
			return path, tok
		}, whyRevoked},
		{"exited", func(g *sessionRig, t *testing.T) (string, string) {
			tok := g.mint(t, "dev")
			g.advance(sessionVerify)
			g.setStart("999")
			return path, tok
		}, whyExited},
		{"out of scope", func(g *sessionRig, t *testing.T) (string, string) { return "/p/qa/v1/messages", g.mint(t, "dev") }, "dev only"},
	}
}

// checkRefusal asserts one refused call names its cause in the response and the log and leaks no token text.
func checkRefusal(t *testing.T, g *sessionRig, log *lockedBuf, path, tok, want string) {
	t.Helper()
	code, body := g.call(t, path, `{}`, tok)
	if code != http.StatusUnauthorized || !strings.Contains(body, want) ||
		!strings.Contains(body, "proxy: missing or wrong client secret") {
		t.Fatalf("response = %d %q, want a 401 naming %q", code, body, want)
	}
	if !strings.Contains(log.String(), "refused 401 POST "+path+": proxy: missing or wrong client secret: ") ||
		!strings.Contains(log.String(), want) {
		t.Fatalf("log %q does not name %q", log.String(), want)
	}
	if tok == "" {
		return
	}
	for _, secretText := range []string{tok, tok[len(sessionTokenPf):], tok[:12], textutil.SHA256Hex([]byte(tok))} {
		if strings.Contains(body, secretText) || strings.Contains(log.String(), secretText) {
			t.Fatalf("token text %q leaked into %q / %q", secretText, body, log.String())
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
			for range 2 { // the first refusal drops the token; the second must still name the cause
				checkRefusal(t, g, &log, path, tok, tc.want)
			}
		})
	}
}

// The remembered drop reasons stay bounded and age out.
func TestDroppedReasonsAreBoundedAndAge(t *testing.T) {
	g := newSessionRig(t)
	for i := range sessionMax + 10 {
		g.r.sessions.mu.Lock()
		g.r.sessions.noteLocked(strings.Repeat("h", i+1), whyExpired)
		g.r.sessions.mu.Unlock()
		g.advance(time.Millisecond)
	}
	if n := len(g.r.sessions.gone); n != sessionMax {
		t.Fatalf("%d reasons kept, want %d", n, sessionMax)
	}
	tok := g.mint(t, "dev")
	g.advance(sessionTTL)
	g.call(t, "/p/dev/v1/messages", `{}`, tok)
	g.advance(droppedKeep)
	if _, body := g.call(t, "/p/dev/v1/messages", `{}`, tok); strings.Contains(body, whyExpired) {
		t.Errorf("a reason older than %v still answers: %q", droppedKeep, body)
	}
}
