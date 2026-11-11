package gateway

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestRevokeSessionTokenPropagatesStateReadFailure(t *testing.T) {
	for _, kind := range []string{"malformed", "unreadable", "missing"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			switch kind {
			case "malformed":
				writeProxyFixture(t, stateFile(), "{")
			case "unreadable":
				writeProxyFixture(t, filepath.Join(stateFile(), "not-a-state-file"), "occupied")
			}
			_, readErr := ReadState()
			err := RevokeSessionToken(context.Background(), "session-to-revoke")
			if kind == "missing" {
				if err != nil {
					t.Fatalf("missing state: revoke = %v, want nil", err)
				}
				return
			}
			if err == nil || errors.Is(err, ErrNotRunning) {
				t.Fatalf("unreadable state silently treated as stopped: %v", err)
			}
			if !strings.Contains(err.Error(), "proxy: read state: "+readErr.Error()) {
				t.Fatalf("revoke lost state read error: %v; want %v", err, readErr)
			}
		})
	}
}

func stubToken(t *testing.T, mint func(context.Context, string, int) (string, error)) {
	t.Helper()
	oe, os_, om := tokenEnsure, tokenState, tokenMint
	t.Cleanup(func() { tokenEnsure, tokenState, tokenMint = oe, os_, om })
	tokenEnsure = func(context.Context) (string, error) { return "http://127.0.0.1:1", nil }
	tokenState = func(context.Context) (State, bool) { return State{PID: 4242}, true }
	tokenMint = mint
}

func TestTokenMintsDaemonBoundSessionToken(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, "CLAUDE_CODE_ROUTER_SECRET", "long-lived-router-secret")
	var gotProfile string
	var gotPID int
	stubToken(t, func(_ context.Context, p string, pid int) (string, error) {
		gotProfile, gotPID = p, pid
		return "cct_abc", nil
	})
	token, err := Token(context.Background(), "dev")
	if err != nil || token != "cct_abc" || gotProfile != "dev" || gotPID != 4242 {
		t.Fatalf("token %q err %v profile %q pid %d", token, err, gotProfile, gotPID)
	}
}

func TestTokenMintFailure(t *testing.T) {
	vaulttest.Use(t)
	boom := errors.New("boom")
	stubToken(t, func(context.Context, string, int) (string, error) { return "", boom })
	token, err := Token(context.Background(), "dev")
	if token != "" || !errors.Is(err, boom) {
		t.Fatalf("token %q err %v", token, err)
	}
}

func TestTokenUnavailableAndNotRunning(t *testing.T) {
	stubToken(t, func(context.Context, string, int) (string, error) {
		t.Fatal("must not mint without a running proxy")
		return "", nil
	})
	tokenEnsure = func(context.Context) (string, error) { return "", errors.New("unavailable") }
	if token, err := Token(context.Background(), "dev"); token != "" || err == nil ||
		err.Error() != "proxy unavailable; run gateway proxy --start" {
		t.Fatalf("token %q err %v", token, err)
	}
	tokenEnsure = func(context.Context) (string, error) { return "http://127.0.0.1:1", nil }
	tokenState = func(context.Context) (State, bool) { return State{}, false }
	if token, err := Token(context.Background(), "dev"); token != "" || err == nil ||
		err.Error() != "proxy not running" {
		t.Fatalf("token %q err %v", token, err)
	}
}
