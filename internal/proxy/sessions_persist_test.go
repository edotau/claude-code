package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

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
