package codex

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

const tokensJSON = `{"tokens":{"id_token":"x"}}`

func resetAuthMemos(t *testing.T) {
	t.Helper()
	clear := func() {
		authStateMu.Lock()
		authStateMemo, authSoftMemo, authSoftUntil = nil, nil, time.Time{}
		authStateMu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// A keyring-held ChatGPT login reads as present on every call, with or without auth.json to stat.
func TestAuthStatusKeyringLoginStaysPresent(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no keyring plane on this OS")
	}
	t.Setenv("CODEX_HOME", t.TempDir())
	resetAuthMemos(t)
	orig := codexKeychainCmd
	codexKeychainCmd = func(context.Context, ...string) ([]byte, error) { return []byte(tokensJSON), nil }
	t.Cleanup(func() { codexKeychainCmd = orig })
	for i := range 2 {
		if st := (codexEngine{}).AuthStatus(); !st.Present || st.Err != nil {
			t.Fatalf("call %d: keyring login not present: %+v", i, st)
		}
	}
}

// With auth.json present the mtime memo answers without touching the keyring, and a logout is seen at once.
func TestAuthStatusKeyedMemoSkipsKeyring(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	resetAuthMemos(t)
	orig := codexKeychainCmd
	codexKeychainCmd = func(context.Context, ...string) ([]byte, error) {
		t.Error("keyring consulted although auth.json holds the login")
		return nil, nil
	}
	t.Cleanup(func() { codexKeychainCmd = orig })
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(tokensJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if st := (codexEngine{}).AuthStatus(); !st.Present {
			t.Fatalf("call %d: auth.json login not present: %+v", i, st)
		}
	}
}

// stubKeyringLogin swaps the keyring and CLI probes: signedIn() decides what the keyring holds.
func stubKeyringLogin(t *testing.T, signedIn func() bool) {
	t.Helper()
	origKC, origSt := codexKeychainCmd, codexStatus
	codexKeychainCmd = func(context.Context, ...string) ([]byte, error) {
		if signedIn() {
			return []byte(tokensJSON), nil
		}
		return nil, nil
	}
	codexStatus = func(context.Context, string) ([]byte, error) { return []byte("Logged in using an API key"), nil }
	t.Cleanup(func() { codexKeychainCmd, codexStatus = origKC, origSt })
}

// A cached keyring login for one CODEX_HOME is not served for another home.
func TestAuthStatusSoftMemoKeyedOnHome(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no keyring plane on this OS")
	}
	resetAuthMemos(t)
	signedIn := true
	stubKeyringLogin(t, func() bool { return signedIn })
	t.Setenv("CODEX_HOME", t.TempDir())
	if st := (codexEngine{}).AuthStatus(); !st.Present {
		t.Fatalf("first home: %+v", st)
	}
	signedIn = false
	t.Setenv("CODEX_HOME", t.TempDir())
	if st := (codexEngine{}).AuthStatus(); st.Present {
		t.Fatalf("second home served the first home's cached login: %+v", st)
	}
}

// A signed-out result is never cached, so a login from another process shows on the next call.
func TestAuthStatusSignedOutNotCached(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no keyring plane on this OS")
	}
	resetAuthMemos(t)
	signedIn := false
	stubKeyringLogin(t, func() bool { return signedIn })
	t.Setenv("CODEX_HOME", t.TempDir())
	if st := (codexEngine{}).AuthStatus(); st.Present {
		t.Fatalf("signed out reads present: %+v", st)
	}
	signedIn = true
	if st := (codexEngine{}).AuthStatus(); !st.Present {
		t.Fatalf("login from another process not seen: %+v", st)
	}
}

// Verify forgets the soft memo so a fresh probe follows it.
func TestVerifyDropsAuthMemo(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no keyring plane on this OS")
	}
	resetAuthMemos(t)
	signedIn := true
	stubKeyringLogin(t, func() bool { return signedIn })
	t.Setenv("CODEX_HOME", t.TempDir())
	if st := (codexEngine{}).AuthStatus(); !st.Present {
		t.Fatalf("seed: %+v", st)
	}
	signedIn = false
	_, _ = (codexEngine{}).Verify(context.Background())
	if st := (codexEngine{}).AuthStatus(); st.Present {
		t.Fatalf("memo survived Verify: %+v", st)
	}
}
