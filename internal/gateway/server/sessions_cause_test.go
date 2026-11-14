package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/textutil"
)

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
