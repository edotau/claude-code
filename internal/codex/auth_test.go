package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const tokensJSON = `{"tokens":{"id_token":"x"}}`

func resetAuthMemos(t *testing.T) {
	t.Helper()
	authState.Drop()
	t.Cleanup(authState.Drop)
}

// A keyring-held ChatGPT login reads as present on every call, with or without auth.json to stat.
func TestAuthStatusKeyringLoginStaysPresent(t *testing.T) {
	forEachKeyringOS(t, func(t *testing.T) {
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
	})
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

// forEachKeyringOS runs body under each OS with a keyring plane; an unsupported OS never consults the keyring.
func forEachKeyringOS(t *testing.T, body func(t *testing.T)) {
	t.Helper()
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			orig := codexGOOS
			codexGOOS = goos
			t.Cleanup(func() { codexGOOS = orig })
			if goos != "windows" {
				body(t)
				return
			}
			resetAuthMemos(t)
			t.Setenv("CODEX_HOME", t.TempDir())
			stubKeyringLogin(t, func() bool { t.Error("keyring consulted on an OS without a keyring plane"); return true })
			if st := (codexEngine{}).AuthStatus(); st.Present {
				t.Fatalf("keyring login present on %s: %+v", goos, st)
			}
		})
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
	forEachKeyringOS(t, func(t *testing.T) {
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
	})
}

// A signed-out result is never cached, so a login from another process shows on the next call.
func TestAuthStatusSignedOutNotCached(t *testing.T) {
	forEachKeyringOS(t, func(t *testing.T) {
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
	})
}

// Verify forgets the soft memo so a fresh probe follows it.
func TestVerifyDropsAuthMemo(t *testing.T) {
	forEachKeyringOS(t, func(t *testing.T) {
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
	})
}

// With no resolvable home, a ./.codex/auth.json under the cwd is never read as the login.
func TestCodexHomeUnresolvableNeverReadsCwdAuth(t *testing.T) {
	cwd := t.TempDir()
	hosttest.WriteRel(t, cwd, ".codex/auth.json", `{"tokens":{"access_token":"planted"}}`)
	t.Chdir(cwd)
	t.Setenv("HOME", "")
	t.Setenv("CODEX_HOME", "")
	resetAuthMemos(t)
	orig := codexKeychainCmd
	codexKeychainCmd = func(context.Context, ...string) ([]byte, error) { return nil, errors.New("no keyring") }
	t.Cleanup(func() { codexKeychainCmd = orig })
	if tok, _, _ := CodexAccessToken(); tok != "" {
		t.Fatalf("token %q came from the cwd's planted .codex/auth.json", tok)
	}
}

// The codex status line may arrive on stderr; only its first line, cut at " - ", becomes the detail.
func TestCodexDetailFromCombinedOutput(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	pathstest.FakeVendor(t, "codex", "#!/bin/sh\necho 'Logged in using ChatGPT - me@example.com' >&2\necho second line\n")
	p := &providers.Provider{Name: "x", Protocol: providers.ProtocolAnthropic, Auth: providers.Auth{Type: providers.AuthCLI, Login: "codex"}}
	v, d := agent.SigninFor(p).Check(context.Background())
	if v != providers.VerdictOK || d != "Logged in using ChatGPT" {
		t.Errorf("stderr-only status = %s %q", v, d)
	}
}
