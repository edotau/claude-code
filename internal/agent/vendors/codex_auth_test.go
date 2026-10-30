package vendors

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// exitErrorWithCode runs a trivial subprocess that exits with code after writing stderr, returning a genuine
// *exec.ExitError (Stderr populated, as Output() does) — the OS keychain tool's exit semantics without the keyring.
func exitErrorWithCode(t *testing.T, code int, stderr string) *exec.ExitError {
	t.Helper()
	script := "exit " + strconv.Itoa(code)
	if stderr != "" {
		script = "echo " + stderr + " >&2; " + script
	}
	_, err := exec.Command("/bin/sh", "-c", script).Output()
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("exitErrorWithCode(%d): want *exec.ExitError, got %T (%v)", code, err, err)
	}
	return ee
}

// keyringMissErr is the current OS's "nothing stored" exit: security exits 44, secret-tool exits 1 with no stderr.
func keyringMissErr(t *testing.T) *exec.ExitError {
	t.Helper()
	if runtime.GOOS == "darwin" {
		return exitErrorWithCode(t, 44, "")
	}
	return exitErrorWithCode(t, 1, "")
}

// codexAccountJSON is the fixture body the keyring (or file) returns: a well-formed codex auth.json payload.
func codexAccountJSON(accessToken string) string {
	return `{"auth_mode":"chatgpt","tokens":{"access_token":"` + accessToken + `","account_id":"acc_1"}}`
}

// chatgptAccessToken is an unsigned JWT carrying a chatgpt_account_id claim, matching the plans_test fixtures.
var chatgptAccessToken = unsignedJWT(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acc_1"}}`)

// noVendorBin points BinDir() (paths.VendorBin reads $CLAUDE_CONFIG_DIR/bin, not $PATH) at an empty dir so
// paths.VendorBin("codex") never finds a real CLI to shell out to.
func noVendorBin(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
}

func TestCodexAuthJSON_FilePresentKeyringNeverCalled(t *testing.T) {
	noVendorBin(t)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	body := codexAccountJSON(chatgptAccessToken)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	called := false
	orig := codexKeychainCmd
	codexKeychainCmd = func(ctx context.Context, args ...string) ([]byte, error) {
		called = true
		return nil, errors.New("keyring must not be consulted when auth.json is present")
	}
	defer func() { codexKeychainCmd = orig }()

	data, note := codexAuthJSON()
	if string(data) != body {
		t.Errorf("codexAuthJSON data = %q, want %q", data, body)
	}
	if note != "" {
		t.Errorf("codexAuthJSON keyringNote = %q, want empty", note)
	}
	if called {
		t.Error("codexAuthJSON called the keyring seam despite auth.json being present")
	}

	token, accountID, keyringNote := CodexAccessToken()
	if token != chatgptAccessToken || accountID != "acc_1" || keyringNote != "" {
		t.Errorf("CodexAccessToken = (%q, %q, %q), want (%q, %q, \"\")", token, accountID, keyringNote, chatgptAccessToken, "acc_1")
	}
}

func TestCodexAuthJSON_FileAbsentKeyringHit(t *testing.T) {
	noVendorBin(t)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home) // no auth.json written: forces the keyring fallback

	body := codexAccountJSON(chatgptAccessToken)
	orig := codexKeychainCmd
	codexKeychainCmd = func(ctx context.Context, args ...string) ([]byte, error) { return []byte(body), nil }
	defer func() { codexKeychainCmd = orig }()

	token, accountID, keyringNote := CodexAccessToken()
	if token != chatgptAccessToken {
		t.Errorf("CodexAccessToken token = %q, want %q", token, chatgptAccessToken)
	}
	if accountID != "acc_1" {
		t.Errorf("CodexAccessToken accountID = %q, want %q", accountID, "acc_1")
	}
	if keyringNote != "" {
		t.Errorf("CodexAccessToken keyringNote = %q, want empty on a keyring hit", keyringNote)
	}
	if !CodexChatGPTLoginPresent() {
		t.Error("CodexChatGPTLoginPresent() = false, want true when the keyring holds a valid ChatGPT auth blob")
	}
}

func TestCodexAuthJSON_FileAbsentKeyringMissTreatedAsAbsent(t *testing.T) {
	noVendorBin(t)

	cases := []struct {
		name string
		err  error
		out  []byte
	}{
		{"keyring tool: item not found", keyringMissErr(t), nil},
		{"empty output, no error", nil, []byte("")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CODEX_HOME", home)
			orig := codexKeychainCmd
			codexKeychainCmd = func(ctx context.Context, args ...string) ([]byte, error) { return c.out, c.err }
			defer func() { codexKeychainCmd = orig }()

			token, accountID, keyringNote := CodexAccessToken()
			if token != "" || accountID != "" {
				t.Errorf("CodexAccessToken = (%q, %q), want (\"\", \"\") on keyring miss", token, accountID)
			}
			if keyringNote != "" {
				t.Errorf("CodexAccessToken keyringNote = %q, want empty on a plain keyring miss", keyringNote)
			}
			if CodexChatGPTLoginPresent() {
				t.Error("CodexChatGPTLoginPresent() = true, want false on keyring miss")
			}
		})
	}
}

func TestCodexAuthJSON_KeyringErrorOtherThanAbsentNotedAsKeyring(t *testing.T) {
	noVendorBin(t)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)

	orig := codexKeychainCmd
	codexKeychainCmd = func(ctx context.Context, args ...string) ([]byte, error) {
		return nil, exitErrorWithCode(t, 1, "keyring is locked") // non-empty stderr: a real failure on every OS
	}
	defer func() { codexKeychainCmd = orig }()

	_, _, keyringNote := CodexAccessToken()
	if keyringNote == "" {
		t.Error("CodexAccessToken keyringNote = \"\", want a note for a non-absent keyring error")
	}

	err := codexChatGPTLogin()
	if err == nil {
		t.Fatal("codexChatGPTLogin() = nil, want an error surfacing the keyring failure")
	}
	if !strings.Contains(err.Error(), "keyring:") {
		t.Errorf("codexChatGPTLogin() error = %q, want it to mention \"keyring:\"", err)
	}
}

func TestCodexAuthJSON_CorruptKeyringJSONTreatedAsAbsent(t *testing.T) {
	noVendorBin(t)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)

	orig := codexKeychainCmd
	codexKeychainCmd = func(ctx context.Context, args ...string) ([]byte, error) { return []byte("{not json"), nil }
	defer func() { codexKeychainCmd = orig }()

	token, accountID, _ := CodexAccessToken()
	if token != "" || accountID != "" {
		t.Errorf("CodexAccessToken = (%q, %q), want (\"\", \"\") on corrupt keyring JSON", token, accountID)
	}
	if CodexChatGPTLoginPresent() {
		t.Error("CodexChatGPTLoginPresent() = true, want false on corrupt keyring JSON")
	}
}
