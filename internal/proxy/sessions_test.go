package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

func TestResolveProxyTokenCachesPerPidAndPrunesDead(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	om, oa, ost := mintScoped, alive, proxyTokenStart
	t.Cleanup(func() { mintScoped, alive, proxyTokenStart = om, oa, ost })
	alive = func(pid int) bool { return pid == 4242 }
	proxyTokenStart = func(int) (string, error) { return "original-start", nil }
	mints := 0
	mintScoped = func(_ context.Context, scope []string, pid int) (string, error) {
		mints++
		return fmt.Sprintf("cct_%d_%d", pid, mints), nil
	}
	dead := filepath.Join(proxyTokenDir(), "999.json")
	writeProxyFixture(t, dead, "{}")
	ctx, scope := context.Background(), []string{"dev", "prod"}
	first, err := ResolveProxyToken(ctx, scope, 4242)
	if err != nil || first != "cct_4242_1" {
		t.Fatalf("first = %q, %v", first, err)
	}
	if again, _ := ResolveProxyToken(ctx, scope, 4242); again != first || mints != 1 {
		t.Errorf("fresh cache not reused: %q, %d mints", again, mints)
	}
	if wider, _ := ResolveProxyToken(ctx, []string{"dev"}, 4242); wider == first || mints != 2 {
		t.Errorf("scope change reused the cache: %q, %d mints", wider, mints)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Errorf("dead pid cache not pruned: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(proxyTokenDir(), "4242.json")); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Errorf("cache file mode: %v %v", fi, err)
	}
	mintScoped = func(context.Context, []string, int) (string, error) { return "", errors.New("boom") }
	if tok, err := ResolveProxyToken(ctx, []string{"other"}, 4242); err == nil || tok != "" {
		t.Errorf("mint failure = %q, %v; want error and no token", tok, err)
	}
}

func TestResolveProxyTokenRejectsReusedPID(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	oldMint, oldStart := mintScoped, proxyTokenStart
	t.Cleanup(func() { mintScoped, proxyTokenStart = oldMint, oldStart })
	start, mints := "first-process", 0
	proxyTokenStart = func(int) (string, error) { return start, nil }
	mintScoped = func(context.Context, []string, int) (string, error) {
		mints++
		return fmt.Sprintf("process-token-%d", mints), nil
	}
	scope, pid := []string{"dev"}, os.Getpid()
	first, err := ResolveProxyToken(t.Context(), scope, pid)
	if err != nil {
		t.Fatal(err)
	}
	start = "replacement-process"
	second, err := ResolveProxyToken(t.Context(), scope, pid)
	if err != nil || second == first || mints != 2 {
		t.Fatalf("PID reuse: token=%q, mints=%d, err=%v", second, mints, err)
	}
	again, err := ResolveProxyToken(t.Context(), scope, pid)
	if err != nil || again != second || mints != 2 {
		t.Fatalf("same process: token=%q, mints=%d, err=%v", again, mints, err)
	}
}
