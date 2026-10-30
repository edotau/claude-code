// Package signin is the one check -> login -> recheck flow behind every provider kind.
package signin

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
	"github.com/edotau/claude-code/internal/databricks/sdkport"
	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/tui"
	"github.com/edotau/claude-code/internal/agent/vendors"
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
	credential    = providers.Credential
	loginRow      = databricks.LoginRow
	stdinTTY      = tui.StdinIsTerminal
	promptSecret  = hiddenPrompt
	storeSecret   = secrets.Store
	claudeStatus  = providers.ClaudeLoginProbe
	copilotLogin  = vendors.CopilotStoredLogin
	geminiPresent = vendors.GeminiLoginPresent
	runVendor     = runVendorCLI
	codexStatus   = func(ctx context.Context, bin string) ([]byte, error) {
		return exec.CommandContext(ctx, bin, "login", "status").CombinedOutput()
	}
	probeTimeout = 8 * time.Second
)

// hiddenPrompt reads one secret with local echo off (the `vault set` prompt).
func hiddenPrompt(name string) (string, error) {
	return tui.Secret("Paste the value for "+name+" (input hidden, Enter when done): ", secrets.Preview)
}

// For returns the sign-in for p's kind.
func For(p *providers.Provider) Signin {
	m := p.Auth.Method()
	switch {
	case m == providers.AuthNone:
		return none{p}
	case m == providers.AuthCLI && p.Auth.Login == "":
		return claude{}
	case m == providers.AuthCLI || p.Auth.Login != "":
		return vendor{p.Auth.Login}
	case m == providers.AuthOAuth || (p.Preset == providers.PresetDatabricks && m == ""):
		return u2m{p}
	}
	return secret{p}
}

// ValidateSecret rejects a value that cannot be the named credential (shared with `vault set`).
func ValidateSecret(name, value string) error {
	if isGitHubToken(name) && !vendors.ValidCopilotToken(value) {
		return fmt.Errorf("%s not a GitHub token (want gho_/ghp_/ghu_/github_pat_, >=20 chars)", name)
	}
	return tailscale.ValidCredential(name, value)
}

// isGitHubToken: the transitional env name, or a vault name GITHUB_[<PROFILE>_]PAT (providers.SecretName).
func isGitHubToken(name string) bool {
	return name == vendors.CopilotTokenEnv || (strings.HasPrefix(name, "GITHUB_") && strings.HasSuffix(name, "_"+strings.ToUpper(providers.AuthPAT)))
}

type u2m struct{ p *providers.Provider }

func (s u2m) Check(ctx context.Context) (providers.AuthVerdict, string) {
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

func (s u2m) Login(ctx context.Context, _ io.Reader, out io.Writer) error {
	if !s.p.Auth.Sealed() { // cliProfile row: LoginRow would switch it to u2m in the vault
		return fmt.Errorf("%s uses the databricks CLI profile; run `databricks auth login -p %s`", s.p.Name, s.p.CLIProfile)
	}
	return loginRow(ctx, s.p.Name, out)
}

// secret covers sealed (pat, oauth-m2m) and env/file/command credentials.
type secret struct{ p *providers.Provider }

func (s secret) Check(ctx context.Context) (providers.AuthVerdict, string) {
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
	if v == providers.VerdictNeedsLogin && sdkport.IsRejected(err) { // 401 invalid_client: the stored secret is wrong
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

func (s secret) name() string {
	if n := providers.AuthSecretName(s.p); n != "" {
		return n
	}
	return s.p.Auth.Env
}

func (s secret) Login(_ context.Context, _ io.Reader, _ io.Writer) error {
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

// none is a keyless endpoint (ollama): reachable or not.
type none struct{ p *providers.Provider }

func (s none) Check(ctx context.Context) (providers.AuthVerdict, string) {
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

func (none) Login(context.Context, io.Reader, io.Writer) error { return nil }

// claude is the passthrough Claude subscription.
type claude struct{}

func (claude) Check(context.Context) (providers.AuthVerdict, string) {
	ok, plan, err := claudeStatus()
	switch {
	case err != nil:
		return providers.VerdictTransient, "claude auth status failed: " + err.Error()
	case ok:
		return providers.VerdictOK, strings.TrimSuffix("logged in "+strings.Trim(plan, " ()"), " ")
	}
	return providers.VerdictNeedsLogin, ""
}

func (claude) Login(ctx context.Context, in io.Reader, out io.Writer) error {
	return runVendor(ctx, "claude", []string{"auth", "login"}, in, out)
}

// vendor is a passthrough row served by another CLI's own sign-in (codex, copilot, gemini).
type vendor struct{ name string }

func (s vendor) Check(ctx context.Context) (providers.AuthVerdict, string) {
	switch s.name {
	case "codex":
		bin, err := paths.VendorBin("codex")
		if err != nil {
			return providers.VerdictTransient, err.Error()
		}
		ctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		out, err := codexStatus(ctx, bin)
		var ee *exec.ExitError
		switch {
		case err == nil:
			line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
			line, _, _ = strings.Cut(line, " - ") // drop the account/key tail codex appends
			return providers.VerdictOK, line
		case errors.As(err, &ee):
			return providers.VerdictNeedsLogin, ""
		}
		return providers.VerdictTransient, err.Error()
	case "copilot":
		if l := copilotLogin(); l != "" {
			return providers.VerdictOK, "signed in as " + l
		}
		return providers.VerdictNeedsLogin, ""
	case "gemini":
		if geminiPresent() {
			return providers.VerdictOK, "agy login present"
		}
		return providers.VerdictNeedsLogin, ""
	}
	return providers.VerdictTransient, "unknown sign-in " + s.name
}

func (s vendor) Login(ctx context.Context, in io.Reader, out io.Writer) error {
	switch s.name {
	case "codex", "copilot":
		return runVendor(ctx, s.name, []string{"login"}, in, out)
	case "gemini":
		return errors.New("agy has no login subcommand; run `agy` yourself and sign in with Google")
	}
	return fmt.Errorf("no sign-in known for %q", s.name)
}

// runVendorCLI runs the vendor CLI's own login on the caller's terminal (list args, no shell).
func runVendorCLI(ctx context.Context, name string, args []string, in io.Reader, out io.Writer) error {
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
