package gateway

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
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
