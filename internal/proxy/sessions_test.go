package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
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
	code, b := g.call(t, SessionPath, `{"profile":"`+profile+`","pid":4242}`, secret)
	var out sessionResp
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
	if code, _ := g.call(t, SessionPath, `{"profile":"dev","pid":4242}`, tok); code != http.StatusUnauthorized {
		t.Errorf("mint with a session token = %d, want 401", code)
	}
	if code, _ := g.call(t, SessionRevokePath, `{"token":"`+tok+`"}`, tok); code != http.StatusUnauthorized {
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
	for range 3 { // 3 x 8h exceeds the 12h TTL only if use does not renew it
		g.advance(8 * time.Hour)
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
	if c, _ := g.call(t, SessionRevokePath, `{"token":"`+tok+`"}`, secret); c != http.StatusNoContent {
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
	token, err := first.sessions.mint("dev", os.Getpid())
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
	token, err := first.sessions.mint("dev", os.Getpid())
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
	token, err := router.sessions.mint("dev", os.Getpid())
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
