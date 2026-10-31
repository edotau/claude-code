// Signin is the one check -> login -> recheck flow behind every provider kind (signin.go).
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/ui/tui"
)

// Signin checks one provider's credential and, when it is missing or refused, signs in again.
type Signin interface {
	// Check never returns a token in detail; a failed probe is VerdictTransient, never needs_login.
	Check(ctx context.Context) (providers.AuthVerdict, string)
	// Login runs the kind's own sign-in; in and out are the caller's terminal.
	Login(ctx context.Context, in io.Reader, out io.Writer) error
}

// Test seams: the credential mint, the u2m login, the secret prompt and store, the vendor probes.
var (
	credential         = providers.Credential
	loginRow           = databricks.LoginRow
	stdinTTY           = tui.StdinIsTerminal
	promptSecret       = hiddenPrompt
	storeSecret        = secrets.Store
	claudeStatus       = providers.ClaudeLoginProbe
	signinCopilotLogin = CopilotStoredLogin
	geminiPresent      = GeminiLoginPresent
	runLogin           = runLoginCLI
	codexStatus        = func(ctx context.Context, bin string) ([]byte, error) {
		return exec.CommandContext(ctx, bin, "login", "status").CombinedOutput()
	}
	probeTimeout = 8 * time.Second
)

// hiddenPrompt reads one secret with local echo off (the `vault set` prompt).
func hiddenPrompt(name string) (string, error) {
	return tui.Secret("Paste the value for "+name+" (input hidden, Enter when done): ", secrets.Preview)
}

// SigninFor returns the sign-in for p's kind.
func SigninFor(p *providers.Provider) Signin {
	m := p.Auth.Method()
	switch {
	case m == providers.AuthNone:
		return noneSignin{p}
	case m == providers.AuthCLI && p.Auth.Login == "":
		return claudeSignin{}
	case m == providers.AuthCLI || p.Auth.Login != "":
		return cliSignin{p.Auth.Login}
	case m == providers.AuthOAuth || (p.Preset == providers.PresetDatabricks && m == ""):
		return u2mSignin{p}
	}
	return secretSignin{p}
}

// ValidateSecret rejects a value that cannot be the named credential (shared with `vault set`).
func ValidateSecret(name, value string) error {
	if isGitHubToken(name) && !ValidCopilotToken(value) {
		return fmt.Errorf("%s not a GitHub token (want gho_/ghp_/ghu_/github_pat_, >=20 chars)", name)
	}
	return tailscale.ValidCredential(name, value)
}

// isGitHubToken: the transitional env name, or a vault name GITHUB_[<PROFILE>_]PAT (providers.SecretName).
func isGitHubToken(name string) bool {
	return name == CopilotTokenEnv || (strings.HasPrefix(name, "GITHUB_") && strings.HasSuffix(name, "_"+strings.ToUpper(providers.AuthPAT)))
}

type u2mSignin struct{ p *providers.Provider }

func (s u2mSignin) Check(ctx context.Context) (providers.AuthVerdict, string) {
	_, err := credential(ctx, s.p)
	v := providers.Verdict(err)
	switch v {
	case providers.VerdictOK:
		if exp, has := providers.TokenExpiry(s.p.Name); has && time.Until(exp) > 0 {
			return v, fmt.Sprintf("expires in %dm", int(time.Until(exp).Minutes()))
		}
		return v, ""
	case providers.VerdictTransient:
		if !s.p.Auth.Sealed() && s.p.CLIProfile != "" { // the CLI mint cannot tell offline from expired
			return v, fmt.Sprintf("%v; if the CLI login expired: databricks auth login -p %s", err, s.p.CLIProfile)
		}
		return v, err.Error()
	}
	return v, ""
}

func (s u2mSignin) Login(ctx context.Context, _ io.Reader, out io.Writer) error {
	if !s.p.Auth.Sealed() { // cliProfile row: LoginRow would switch it to u2m in the vault
		return fmt.Errorf("%s uses the databricks CLI profile; run `databricks auth login -p %s`", s.p.Name, s.p.CLIProfile)
	}
	return loginRow(ctx, s.p.Name, out)
}

// secretSignin covers sealed (pat, oauth-m2m) and env/file/command credentials.
type secretSignin struct{ p *providers.Provider }

func (s secretSignin) Check(ctx context.Context) (providers.AuthVerdict, string) {
	if s.p.Auth.Sealed() {
		if _, err := providers.HasSecretErr(providers.AuthSecretName(s.p)); err != nil { // a locked vault is not "absent"
			return providers.VerdictTransient, "vault: " + err.Error()
		}
	}
	if !providers.CredentialPresent(s.p) {
		return providers.VerdictNeedsLogin, "no credential"
	}
	_, err := credential(ctx, s.p)
	v := providers.Verdict(err)
	if v == providers.VerdictNeedsLogin && oauth.IsRejected(err) { // 401 invalid_client: the stored secret is wrong
		return providers.VerdictRejected, err.Error()
	}
	if v == providers.VerdictTransient {
		return v, err.Error()
	}
	if v == providers.VerdictOK && !s.p.Auth.Sealed() {
		return v, "key present, not validated"
	}
	return v, ""
}

func (s secretSignin) name() string {
	if n := providers.AuthSecretName(s.p); n != "" {
		return n
	}
	return s.p.Auth.Env
}

func (s secretSignin) Login(_ context.Context, _ io.Reader, _ io.Writer) error {
	name := s.name()
	if name == "" {
		return fmt.Errorf("%s: no vault secret to set; edit its auth.command or auth.file", s.p.Name)
	}
	if !stdinTTY() {
		return fmt.Errorf("not a terminal; run `claude-code vault set %s`", name)
	}
	v, err := promptSecret(name)
	if err != nil {
		return err
	}
	if v = strings.TrimSpace(v); v == "" {
		return errors.New("empty value")
	}
	if err := ValidateSecret(name, v); err != nil {
		return err
	}
	if err := storeSecret(name, v); err != nil {
		return err
	}
	_ = router.ClearProviderAuth(s.p.Name) // best-effort: the TTL backstops a missed clear
	return nil
}

// probeClient is shared by every keyless-endpoint probe (no proxy).
var probeClient = &http.Client{Transport: &http.Transport{Proxy: nil}}

// noneSignin is a keyless endpoint (ollama): reachable or not.
type noneSignin struct{ p *providers.Provider }

func (s noneSignin) Check(ctx context.Context) (providers.AuthVerdict, string) {
	if s.p.BaseURL == "" {
		return providers.VerdictOK, ""
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.p.BaseURL, nil)
	if err != nil {
		return providers.VerdictTransient, err.Error()
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return providers.VerdictTransient, "unreachable: " + err.Error()
	}
	_ = resp.Body.Close()
	return providers.VerdictOK, ""
}

func (noneSignin) Login(context.Context, io.Reader, io.Writer) error { return nil }

// claudeSignin is the passthrough Claude subscription.
type claudeSignin struct{}

func (claudeSignin) Check(context.Context) (providers.AuthVerdict, string) {
	ok, plan, err := claudeStatus()
	switch {
	case err != nil:
		return providers.VerdictTransient, "claude auth status failed: " + err.Error()
	case ok:
		return providers.VerdictOK, strings.TrimSuffix("logged in "+strings.Trim(plan, " ()"), " ")
	}
	return providers.VerdictNeedsLogin, ""
}

func (claudeSignin) Login(ctx context.Context, in io.Reader, out io.Writer) error {
	return runLogin(ctx, "claude", []string{"auth", "login"}, in, out)
}

// cliSignin is a passthrough row served by another CLI's own sign-in (codex, copilot, gemini).
type cliSignin struct{ name string }

func (s cliSignin) Check(ctx context.Context) (providers.AuthVerdict, string) {
	return schemaFor(s.name).eng().checkLogin(ctx)
}

func (s cliSignin) Login(ctx context.Context, in io.Reader, out io.Writer) error {
	row, ok := LookupSchema(s.name)
	switch {
	case !ok || len(row.CLI.LoginArgv) == 0:
		return fmt.Errorf("no sign-in known for %q", s.name)
	case len(row.CLI.LoginArgv) == 1:
		bin := row.CLI.LoginArgv[0]
		return fmt.Errorf("%s has no login subcommand; run `%s` yourself and sign in with Google", bin, bin)
	}
	return runLogin(ctx, s.name, row.CLI.LoginArgv[1:], in, out)
}

// runLoginCLI runs the vendor CLI's own login on the caller's terminal (list args, no shell).
func runLoginCLI(ctx context.Context, name string, args []string, in io.Reader, out io.Writer) error {
	bin, err := paths.VendorBin(name)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, out
	if in == nil {
		cmd.Stdin = os.Stdin
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}
