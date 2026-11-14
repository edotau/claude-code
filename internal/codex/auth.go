package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
)

// Verify runs `codex login status`; a deadline or a killed process is transient, never "not signed in".
func (codexEngine) Verify(ctx context.Context) (providers.AuthVerdict, string) {
	authState.Drop() // forget every memoized AuthStatus so the next call re-reads the login
	bin, err := paths.AgentBin("codex")
	if err != nil {
		return providers.VerdictTransient, err.Error()
	}
	ctx, cancel := context.WithTimeout(ctx, agent.ProbeTimeout)
	defer cancel()
	out, err := codexStatus(ctx, bin)
	if err != nil {
		return agent.ClassifyProbe(ctx, err)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	line, _, _ = strings.Cut(line, " - ") // drop the account/key tail codex appends
	return providers.VerdictOK, line
}

var _ agent.Authenticator = codexEngine{}

// codexAuthSource is the AuthState.Source of a ChatGPT sign-in.
const codexAuthSource = "ChatGPT login (codex login)"

// authKey identifies one version of auth.json for the memos below.
type authKey struct {
	path  string
	mtime time.Time
	size  int64
}

// authFileKey stats $CODEX_HOME/auth.json; ok is false when it is absent, and the key is then the bare path.
func authFileKey() (authKey, bool) {
	path := filepath.Join(codexHome(), "auth.json")
	fi, err := os.Stat(path)
	if err != nil {
		return authKey{path: path}, false
	}
	return authKey{path, fi.ModTime(), fi.Size()}, true
}

var (
	authData  paths.Memo[authKey, []byte]
	authState paths.Memo[authKey, providers.AuthState] // one probe per key at a time; a bare-path key is the soft entry
)

// authSoftTTL bounds how long a keyring-backed signed-in result is reused without an auth.json to stat.
const authSoftTTL = 5 * time.Second

// codexTokens is the "tokens" object of an auth.json body.
type codexTokens struct {
	IDToken string `json:"id_token"`
	Access  string `json:"access_token"`
	Account string `json:"account_id"`
}

// parseCodexTokens decodes an auth.json body; present is a non-null "tokens" object, t is zero when unreadable.
func parseCodexTokens(b []byte) (t codexTokens, present bool) {
	var auth struct {
		Tokens json.RawMessage `json:"tokens"`
	}
	if b == nil || json.Unmarshal(b, &auth) != nil || len(auth.Tokens) == 0 || string(auth.Tokens) == "null" {
		return codexTokens{}, false
	}
	if json.Unmarshal(auth.Tokens, &t) != nil {
		return codexTokens{}, true
	}
	return t, true
}

// AuthStatus reads auth.json or the keyring first (memoized by mtime); `codex login status` runs only as fallback.
func (codexEngine) AuthStatus() providers.AuthState {
	key, keyed := authFileKey()
	st, _ := authState.Do(key, func(st providers.AuthState, _ error) time.Duration {
		switch {
		case st.Err != nil:
			return -1
		case keyed:
			return 0
		case st.Present:
			return authSoftTTL
		}
		return -1
	}, func() (providers.AuthState, error) { return codexAuthState(), nil })
	return st
}

func codexAuthState() providers.AuthState {
	b, keyringNote := codexAuthJSON()
	t, present := parseCodexTokens(b)
	if present {
		return providers.AuthState{Present: true, Source: codexAuthSource, Method: providers.AuthCLI, PlanHint: codexPlanFrom(t)}
	}
	bin, err := paths.AgentBin("codex")
	if err != nil { // no CLI to ask: a failed keyring lookup is the only unknown left
		if keyringNote != "" {
			return providers.AuthState{Method: providers.AuthNone, Err: errors.New("codex keyring: " + keyringNote)}
		}
		return providers.AuthState{Method: providers.AuthNone}
	}
	verdict, transient := codexLoginProbe(bin)
	switch {
	case transient:
		return providers.AuthState{Method: providers.AuthNone, Err: verdict}
	case verdict == nil:
		return providers.AuthState{Present: true, Source: codexAuthSource, Method: providers.AuthCLI}
	}
	return providers.AuthState{Method: providers.AuthNone} // an API key or no login: codex answered
}

// Plan is codex's ChatGPT plan ("Plus", "Pro"): st.PlanHint, which AuthStatus read from the login's id token.
func (codexEngine) Plan(_ context.Context, st providers.AuthState) providers.Subscription {
	return providers.Subscription{Raw: st.PlanHint, Name: textutil.Title(st.PlanHint)}
}

// codexKeychainService is the macOS/Linux keyring service codex stores its ChatGPT login under when
// config.toml sets cli_auth_credentials_store = "keyring" (the whole auth.json body as the secret value).
const codexKeychainService = "Codex Auth"

// codexGOOS is the OS whose keyring plane the lookups below use; a seam for tests.
var codexGOOS = runtime.GOOS

// codexKeychainCmd runs the OS secret-store lookup (security on macOS, secret-tool on Linux); tests replace it.
var codexKeychainCmd = func(ctx context.Context, args ...string) ([]byte, error) {
	name := "security"
	if codexGOOS == "linux" {
		name = "secret-tool"
	}
	return exec.CommandContext(ctx, name, args...).Output()
}

// codexAuthJSON is codex's ChatGPT login payload, $CODEX_HOME/auth.json first and the OS keyring second —
// codex writes there instead of the file once config.toml sets cli_auth_credentials_store = "keyring", but
// that setting is only a hint: the file is always tried first regardless of what it says. data is nil
// when neither holds a login; keyringNote is "" normally, or the reason a keyring lookup
// failed for something other than "nothing stored", for a caller that wants to say why.
func codexAuthJSON() (data []byte, keyringNote string) {
	if b, ok := codexAuthFile(); ok {
		return b, ""
	}
	return codexKeyringAuthJSON()
}

// codexAuthFile is auth.json's body, re-read only when its path, mtime or size changes.
func codexAuthFile() ([]byte, bool) {
	key, ok := authFileKey()
	if !ok {
		return nil, false
	}
	b, err := authData.Do(key, func(_ []byte, err error) time.Duration {
		if err != nil {
			return -1
		}
		return 0
	}, func() ([]byte, error) { return os.ReadFile(key.path) })
	return b, err == nil
}

// codexKeyringAuthJSON looks up the "Codex Auth" keyring item; the first of several accounts answers (codex keeps one).
func codexKeyringAuthJSON() (data []byte, note string) {
	var args []string
	switch codexGOOS {
	case "darwin":
		args = []string{"find-generic-password", "-s", codexKeychainService, "-w"}
	case "linux":
		args = []string{"lookup", "service", codexKeychainService}
	default:
		return nil, "" // no keyring plane wired up for this OS
	}
	ctx, cancel := context.WithTimeout(context.Background(), agent.ProbeTimeout)
	defer cancel()
	out, err := codexKeychainCmd(ctx, args...)
	if err == nil {
		if v := strings.TrimSpace(string(out)); v != "" {
			return out, ""
		}
		return nil, "" // empty value reads as nothing stored
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		if codexKeyringNotFound(exit) {
			return nil, ""
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

// codexPlanFrom is the ChatGPT plan ("plus", "pro", "team"…) in the id_token claim of an auth.json body; "" when
// absent or unreadable. The token is decoded, never verified or printed.
func codexPlanFrom(t codexTokens) string {
	parts := strings.Split(t.IDToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			Plan string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Auth.Plan
}

// CodexAccessToken is codex's ChatGPT access token and account id from its auth.json (file or keyring);
// "" when absent. keyringNote is "" normally, or the reason a keyring lookup failed for something other
// than "nothing stored", for callers that want to say why no token was found.
func CodexAccessToken() (token, accountID, keyringNote string) {
	b, note := codexAuthJSON()
	if b == nil {
		return "", "", note
	}
	t, _ := parseCodexTokens(b)
	return t.Access, t.Account, ""
}

// codexLoginProbe runs `codex login status` under the probe timeout and classifies it.
func codexLoginProbe(bin string) (error, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), agent.ProbeTimeout)
	defer cancel()
	out, err := codexStatus(ctx, bin)
	return codexLoginVerdict(ctx, out, err)
}

func codexLoginVerdict(ctx context.Context, out []byte, err error) (error, bool) {
	text, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	text, _, _ = strings.Cut(text, " - ")
	ee, exited := errors.AsType[*exec.ExitError](err)
	switch {
	case err == nil && strings.Contains(strings.ToLower(text), "chatgpt"):
		return nil, false
	case err == nil:
		return fmt.Errorf("codex: signed in with an API key, not ChatGPT (%s); run `codex login` and choose \"Sign in with ChatGPT\"", text), false
	case ctx.Err() == nil && exited && ee.ExitCode() >= 0:
		return fmt.Errorf("codex: not signed in (%s); run `codex login` and choose \"Sign in with ChatGPT\"", text), false
	case ctx.Err() != nil:
		return fmt.Errorf("codex login status: %w", ctx.Err()), true
	default:
		return fmt.Errorf("codex login status: %w", err), true
	}
}

func codexKeyringNotFound(exit *exec.ExitError) bool {
	switch codexGOOS {
	case "darwin":
		return exit.ExitCode() == 44 // errSecItemNotFound
	case "linux":
		return exit.ExitCode() == 1 && strings.TrimSpace(string(exit.Stderr)) == ""
	default:
		return false
	}
}
