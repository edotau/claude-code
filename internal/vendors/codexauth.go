package vendors

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Where codex's ChatGPT auth.json payload came from.
const (
	codexAuthFile    = "file"
	codexAuthKeyring = "keyring"
)

// codexKeychainService is the macOS/Linux keyring service codex stores its ChatGPT login under when
// config.toml sets cli_auth_credentials_store = "keyring" (the whole auth.json body as the secret value).
const codexKeychainService = "Codex Auth"

// codexKeychainCmd runs the OS secret-store lookup (security on macOS, secret-tool on Linux); tests
// replace it so none of this reaches the real keychain.
var codexKeychainCmd = func(ctx context.Context, args ...string) ([]byte, error) {
	name := "security"
	if runtime.GOOS == "linux" {
		name = "secret-tool"
	}
	return exec.CommandContext(ctx, name, args...).Output()
}

// codexAuthJSON is codex's ChatGPT login payload, $CODEX_HOME/auth.json first and the OS keyring second —
// codex writes there instead of the file once config.toml sets cli_auth_credentials_store = "keyring", but
// that setting is only a hint: the file is always tried first regardless of what it says. data is nil and
// source is "" when neither holds a login; keyringNote is "" normally, or the reason a keyring lookup
// failed for something other than "nothing stored", for a caller that wants to say why.
func codexAuthJSON() (data []byte, source, keyringNote string) {
	if b, err := os.ReadFile(filepath.Join(codexHome(), "auth.json")); err == nil {
		return b, codexAuthFile, ""
	}
	if b, note := codexKeyringAuthJSON(); b != nil {
		return b, codexAuthKeyring, ""
	} else if note != "" {
		return nil, "", note
	}
	return nil, "", ""
}

// codexKeyringAuthJSON looks up the "Codex Auth" keyring item; an item with several accounts answers
// with the first, which is fine since codex only ever stores one ChatGPT login at a time.
func codexKeyringAuthJSON() (data []byte, note string) {
	var args []string
	switch runtime.GOOS {
	case "darwin":
		args = []string{"find-generic-password", "-s", codexKeychainService, "-w"}
	case "linux":
		args = []string{"lookup", "service", codexKeychainService}
	default:
		return nil, "" // no keyring plane wired up for this OS
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := codexKeychainCmd(ctx, args...)
	if err == nil {
		if v := strings.TrimSpace(string(out)); v != "" {
			return out, ""
		}
		return nil, "" // empty value reads as nothing stored
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		switch runtime.GOOS {
		case "darwin":
			if exit.ExitCode() == 44 { // errSecItemNotFound
				return nil, ""
			}
		case "linux":
			if exit.ExitCode() == 1 && strings.TrimSpace(string(exit.Stderr)) == "" {
				return nil, "" // secret-tool's plain "not found" exit
			}
		}
		if stderr := strings.TrimSpace(string(exit.Stderr)); stderr != "" {
			return nil, stderr
		}
	}
	if ctx.Err() != nil {
		return nil, "timed out"
	}
	return nil, err.Error()
}
