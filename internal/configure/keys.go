// Package configure is the `claude-code configure` dialogue: provider, keys, models, agents
// and more sections over the existing provider/secrets/agents verbs, piped or interactive.
// It is also agent.OfferConfigure's auto-configure callback, scoped to one agent via Agent.
package configure

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// WarmServedOnChange warms the served cache when a pin changed or the switch flipped on since the run's snapshot.
func WarmServedOnChange(out io.Writer, skip bool, pins map[string]router.PinSpec, wasOn bool) {
	if skip || !providers.ServedRoutingOn() {
		return
	}
	current, err := agent.AgentPins()
	if err != nil {
		fmt.Fprintf(out, "configure: %v\n", err)
		return
	}
	if wasOn && maps.Equal(pins, current) {
		return
	}
	if reg, err := providers.Load(); err == nil {
		preflight.Warm(out, reg, time.Now())
	}
}

// CurrentProvider is the configured profile (vault pin), falling back to the registry default when unpinned.
func CurrentProvider(reg *providers.Registry) string {
	if cur := vault.GetAgentRecord("claude").Profile; cur != "" {
		return cur
	}
	return reg.Default
}

// KeyTarget is one row `configure keys` reports on: a provider-level credential or an agent's own login.
type KeyTarget struct {
	Label     string
	Present   bool
	Source    string              // present: which source/login answered
	LoginArgv []string            // absent + a login CLI exists: offer/run this
	EnvVar    string              // absent + API-key based: prompt writes this var
	FixCmd    string              // piped: the exact command to run instead
	Provider  *providers.Provider // present + keyed: probed live so a stored-but-rejected key is not "present"
	MintArgv  []string            // Max plan: the CLI that mints a long-lived headless token (claude setup-token)
	Headless  bool                // Max plan: a setup-token is stored, so unattended runs survive /login expiry
}

// SetupTokenRE matches the long-lived token `claude setup-token` prints once the browser flow completes.
var SetupTokenRE = regexp.MustCompile(`sk-ant-oat\d+-[A-Za-z0-9_-]{20,}`)

// MaxPlanTarget fills the claude row for the Claude Max plan (passthrough): the interactive /login session as
// reported by `claude auth status`, plus whether a long-lived setup-token is stored for unattended runs.
func MaxPlanTarget(kt KeyTarget) KeyTarget {
	kt.LoginArgv = []string{"claude", "auth", "login"}
	kt.MintArgv = []string{"claude", "setup-token"}
	kt.EnvVar = providers.OAuthTokenEnv
	kt.FixCmd = "claude auth login (interactive) · claude setup-token → claude-code vault set " + providers.OAuthTokenEnv + " (headless)"
	kt.Headless = providers.HeadlessOAuthToken() != ""
	st := agent.AuthStatus("claude")
	plan := ""
	if st.PlanHint != "" {
		plan = " (" + st.PlanHint + ")"
	}
	kt.Present = st.Present || kt.Headless
	switch {
	case st.Present && kt.Headless:
		kt.Source = "claude.ai login" + plan + "; headless token set"
	case st.Present:
		kt.Source = "claude.ai login" + plan + "; no headless token"
	case kt.Headless:
		kt.Source = "headless token (" + providers.OAuthTokenEnv + "); not logged in interactively"
	}
	return kt
}

// ConsoleURLs are where each API key is minted; the key option offers to open the page before the paste.
var ConsoleURLs = map[string]string{
	"ANTHROPIC_API_KEY": "https://console.anthropic.com/settings/keys",
	"OPENAI_API_KEY":    "https://platform.openai.com/api-keys",
}

// AuthOption is one way to satisfy an absent credential: an agent's browser/device login, or a pasted key.
type AuthOption struct {
	Action string   // one or two words, the picker's first column (Paste API key)
	Desc   string   // one short line, the second column
	Argv   []string // login CLI to run attached to the terminal
	Mint   []string // CLI that prints a long-lived token; captured and stored under env
	Env    string   // name a pasted (or minted) key is stored under in the vault
}

// Label is the piped one-line form of the option.
func (o AuthOption) Label() string { return o.Action + " — " + o.Desc }

// AuthOptions lists every way kt can be satisfied, browser login first; the dialog shows them all.
func AuthOptions(kt KeyTarget) []AuthOption {
	var opts []AuthOption
	if len(kt.LoginArgv) > 0 {
		opts = append(opts, AuthOption{Action: "Browser login", Desc: strings.Join(kt.LoginArgv, " "), Argv: kt.LoginArgv})
	}
	if len(kt.MintArgv) > 0 {
		opts = append(opts, AuthOption{Action: "Mint token", Desc: strings.Join(kt.MintArgv, " ") + " (unattended runs)",
			Mint: kt.MintArgv, Env: kt.EnvVar})
	}
	if kt.EnvVar != "" {
		desc := kt.EnvVar
		if u := ConsoleURLs[kt.EnvVar]; u != "" {
			desc = "create at " + strings.TrimPrefix(u, "https://")
		}
		opts = append(opts, AuthOption{Action: "Paste API key", Desc: desc, Env: kt.EnvVar})
	}
	return opts
}

func KeyTargets(reg *providers.Registry, agentFilter string) ([]KeyTarget, error) {
	var out []KeyTarget
	if agentFilter == "" {
		sel, err := providers.Select(reg, "")
		if err != nil {
			return nil, err
		}
		p := sel.Provider
		fix := "resolve the credential for " + p.Name + " (see `claude-code configure provider --list`)"
		if p.Auth.Env != "" {
			fix = "claude-code vault set " + p.Auth.Env
		}
		kt := KeyTarget{
			Label: "claude", Present: providers.CredentialPresent(p), Source: providers.Resolve(p).Source,
			EnvVar: p.Auth.Env, FixCmd: fix, Provider: p,
		}
		if p.Auth.Method() == providers.AuthCLI {
			kt = MaxPlanTarget(kt)
		}
		out = append(out, kt)
	}
	for _, name := range agent.Names() {
		if agentFilter != "" && name != agentFilter {
			continue
		}
		out = append(out, AgentKeyTarget(reg, name))
	}
	return out, nil
}

func AgentKeyTarget(reg *providers.Registry, name string) KeyTarget {
	s, known := agent.LookupSchema(name)
	if st := agent.AuthStatus(name); st.Present {
		kt := KeyTarget{Label: name, Present: true, Source: st.Label()}
		if st.Method == providers.AuthCLI { // a gh account or env token has no sign-in to re-run
			kt.LoginArgv = s.CLI.LoginArgv // rotate: re-run the CLI's own sign-in
		}
		return kt
	}
	if name == "gemini" { // agy has no key route, so nothing to fall back to
		return KeyTarget{Label: name, Present: false, LoginArgv: s.CLI.LoginArgv, FixCmd: "run `agy` to sign in with Google"}
	}
	kt := fallbackKeyTarget(reg, name, s, known)
	if !kt.Present {
		kt.LoginArgv = s.CLI.LoginArgv
	}
	return kt
}

// fallbackKeyTarget is the key target for an agent whose own sign-in is absent: the selected provider's key
// when it is usable, the agent's own stored login, or the configure hint.
func fallbackKeyTarget(reg *providers.Registry, name string, s agent.Schema, known bool) KeyTarget {
	if sel, err := providers.Select(reg, ""); err == nil { // only when the agent's own sign-in is absent
		target, note, rerr := s.ResolveTarget(reg, sel, false)
		// The agent's own stored config/login is a working setup, not a missing key.
		if errors.Is(rerr, agent.ErrOwnLogin) {
			return KeyTarget{Label: name, Present: true, Source: AfterUsing(note)}
		}
		// A passthrough provider carries another agent's login, never a key this agent can use.
		if rerr == nil && target.Provider.Auth.Method() != providers.AuthCLI && providers.CredentialPresent(target.Provider) {
			return KeyTarget{Label: name, Present: true, Source: target.Provider.Name + " key",
				EnvVar: target.Provider.Auth.Env, Provider: target.Provider}
		}
	}
	hint := "run `claude-code configure agents`"
	if known {
		hint = s.CLI.ConfigureHint
	}
	return KeyTarget{Label: name, Present: false, FixCmd: hint}
}

// Probe live-checks a present keyed credential (4s cap); "" when there is nothing to Probe.
func Probe(kt KeyTarget) (status, detail string) {
	if kt.Provider == nil {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	return providers.ProbeCredential(ctx, kt.Provider)
}

// probeTTL is how long a verified/rejected probe result is trusted before re-probing; "unreachable" is never cached.
const probeTTL = 6 * time.Hour

// ProbeCached reuses fresh vault probe results; env overrides and uncached values always probe live.
func ProbeCached(kt KeyTarget) (status, detail string, age time.Duration) {
	if kt.Provider == nil || kt.Provider.Auth.Env == "" {
		status, detail = Probe(kt)
		return status, detail, 0
	}
	env := kt.Provider.Auth.Env
	meta, src := probeSource(env)
	if src != "vault" {
		status, detail = Probe(kt)
		return status, detail, 0
	}
	if cached, cachedAge, ok := vault.CachedProbe(meta[env], probeTTL, time.Now()); ok {
		return cached, "", cachedAge
	}
	setAt := meta[env].SetAt
	status, detail = Probe(kt)
	if status == "verified" || status == "rejected" {
		_ = vault.RecordProbe(env, status, time.Now(), setAt) // a failed cache write leaves the live result intact
	}
	return status, detail, 0
}

// probeSource resolves env's launch source; unreadable vault metadata yields nil, forcing a live probe.
func probeSource(env string) (meta map[string]vault.SecretMeta, src string) {
	meta, err := vault.LoadMeta()
	if err != nil {
		_, src, _, _ = vault.Effective(env) // meta unreadable: fall back to the live-lookup ladder
		return nil, src
	}
	src, _ = vault.Locate(env, meta)
	return meta, src
}

// AfterUsing extracts the fallback description from a ResolveTarget note ("…; using <this>").
func AfterUsing(note string) string {
	if _, after, ok := strings.Cut(note, "using "); ok {
		return after
	}
	return "its own login"
}

// RunMint tees stdout from the token-minting CLI and returns its token only when the command succeeds.
func RunMint(argv []string) (string, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return RunMintContext(ctx, argv)
}

// attachedCommand is argv under ctx; cancel sends SIGINT first, and Go kills it after WaitDelay.
func attachedCommand(ctx context.Context, argv []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 3 * time.Second
	return cmd
}

// RunMintContext is RunMint under ctx; the caller asks first, a stray Enter must never start a mint.
func RunMintContext(ctx context.Context, argv []string) (string, error) {
	if err := ctxErr(ctx, "mint token"); err != nil {
		return "", err
	}
	argv, err := cliArgv(argv)
	if err != nil {
		return "", err
	}
	defer proc.Preserve()()
	var buf bytes.Buffer
	cmd := attachedCommand(ctx, argv)
	cmd.Stdin, cmd.Stderr = os.Stdin, os.Stderr
	cmd.Stdout = io.MultiWriter(os.Stdout, &buf)
	err = cmd.Run()
	if cerr := ctxErr(ctx, "mint token"); cerr != nil {
		return "", cerr
	}
	if err != nil {
		return "", err
	}
	DiscoverAfterSignin()
	return SetupTokenRE.FindString(buf.String()), nil
}

// DiscoverAfterSignin refreshes copilot's and opencode's model lists in the background after a successful sign-in.
func DiscoverAfterSignin() {
	if reg, err := providers.Load(); err == nil {
		preflight.DiscoverAfterAuth(reg)
	}
}

// ctxErr wraps ctx's error under verb when ctx is already done, else nil — the one cancellation check
// shared by every exec.CommandContext caller here and in home.go's registerClaudeMCPContext.
func ctxErr(ctx context.Context, verb string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", verb, err)
	}
	return nil
}

// RunLogin execs an agent's own login CLI attached to the real terminal: picking the row that leads here
// already was the confirmation, so it runs directly rather than asking [y/N] a second time.
func RunLogin(argv []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return RunLoginContext(ctx, argv)
}

// cliArgv resolves a known agent CLI in argv[0] to its installed binary path; other commands pass through.
func cliArgv(argv []string) ([]string, error) {
	name, ok := paths.AgentName(argv[0])
	if !ok {
		return argv, nil
	}
	bin, err := paths.AgentBin(name)
	if err != nil {
		return nil, err
	}
	resolved := slices.Clone(argv)
	resolved[0] = bin
	return resolved, nil
}

func RunLoginContext(ctx context.Context, argv []string) error {
	argv, err := cliArgv(argv)
	if err != nil {
		return err
	}
	defer proc.Preserve()()
	cmd := attachedCommand(ctx, argv)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	if cerr := ctxErr(ctx, "login"); cerr != nil {
		return cerr
	}
	if err == nil {
		DiscoverAfterSignin()
	}
	return err
}

// ProbeValue live-checks a candidate value for p without storing it first, via the env-var seam
// providers.Credential already checks ahead of the vault; the previous env is restored after.
func ProbeValue(p *providers.Provider, envVar, candidate string) (status, detail string) {
	prev, had := os.LookupEnv(envVar)
	if err := os.Setenv(envVar, candidate); err != nil {
		return "unreachable", fmt.Sprintf("cannot set %s for credential probe: %v", envVar, err)
	}
	defer func() {
		if had {
			_ = os.Setenv(envVar, prev)
		} else {
			_ = os.Unsetenv(envVar)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	return providers.ProbeCredential(ctx, p)
}

// RotateOptions lists how an already-present kt can be rotated: its own CLI first (login, then mint —
// the anthropic subscription row carries both, named explicitly), else a plain paste when it is a
// keyed credential with no generator.
func RotateOptions(kt KeyTarget) []AuthOption {
	var opts []AuthOption
	if len(kt.LoginArgv) > 0 {
		opts = append(opts, AuthOption{Action: "Login", Desc: strings.Join(kt.LoginArgv, " ") + " · browser sign-in, refreshes the session", Argv: kt.LoginArgv})
	}
	if len(kt.MintArgv) > 0 {
		opts = append(opts, AuthOption{Action: "OAuth token", Desc: strings.Join(kt.MintArgv, " ") + " · mints a new token, rotates " + kt.EnvVar,
			Mint: kt.MintArgv, Env: kt.EnvVar})
	}
	if len(opts) == 0 && kt.EnvVar != "" && kt.Provider != nil {
		opts = append(opts, AuthOption{Action: "API key", Desc: "paste a new " + kt.EnvVar + " · rotates the stored key", Env: kt.EnvVar})
	}
	return opts
}

// TrackedSecret is one row of the secrets dialog: a name the harness knows of and where it resolves today.
type TrackedSecret struct {
	Name     string
	Source   string // vault|env|"" (missing)
	Shadowed string // vault|"" — a stored value an env var is shadowing
	Users    string // providers that read it, or a short role
}

// TrackedSecrets combines provider auth.env names, the setup-token, and every secret stored in the vault.
func TrackedSecrets(reg *providers.Registry) []TrackedSecret {
	users := map[string][]string{
		providers.OAuthTokenEnv: {"claude -p (setup-token)"},
	}
	for _, n := range reg.Names() {
		if env := reg.Providers[n].Auth.Env; env != "" {
			users[env] = append(users[env], n)
		}
	}
	meta, metaErr := vault.LoadMeta()
	addCustomUsers(users, meta, metaErr)
	out := make([]TrackedSecret, 0, len(users))
	for name, u := range users {
		src, shadowed := locateSecret(name, meta, metaErr)
		out = append(out, TrackedSecret{Name: name, Source: src, Shadowed: shadowed, Users: strings.Join(u, ", ")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// addCustomUsers marks every stored secret no provider reads as "custom".
func addCustomUsers(users map[string][]string, meta map[string]vault.SecretMeta, metaErr error) {
	var names []string
	if metaErr != nil {
		// Names reads vault names without decoding records, which may still work when metadata is unreadable.
		names = vault.Names()
	} else {
		for env := range meta {
			names = append(names, env)
		}
	}
	for _, env := range names {
		if _, ok := users[env]; !ok {
			users[env] = []string{"custom"}
		}
	}
}

// locateSecret resolves where name comes from today and any vault value an env var shadows.
func locateSecret(name string, meta map[string]vault.SecretMeta, metaErr error) (src, shadowed string) {
	if metaErr == nil {
		return vault.Locate(name, meta)
	}
	var err error
	if _, src, err = vault.EffectiveSecret(name); err != nil {
		src = "unreadable"
	} else if src == "env" {
		_, _, shadowed, _ = vault.Effective(name)
	}
	return src, shadowed
}

// ParseAssignment splits a pasted secret: `export NAME="value"`, `NAME=value`, or a bare value (name "").
// Surrounding quotes and whitespace are stripped; a bare value containing '=' (base64 padding) stays whole.
func ParseAssignment(raw string) (name, value string) {
	s := strings.TrimSpace(raw)
	s = strings.TrimSpace(strings.TrimPrefix(s, "export "))
	if k, v, ok := strings.Cut(s, "="); ok && strings.Trim(v, "=") != "" && vault.EnvName.MatchString(k) && strings.ToUpper(k) == k {
		name, s = k, strings.TrimSpace(v)
	}
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		s = s[1 : len(s)-1]
	}
	return name, s
}

// ValidateSecret checks menu-entered credentials before staging; environment edits retain vault-only validation.
func ValidateSecret(name, value string) error {
	if name == providers.CopilotTokenEnv && !agent.ValidCopilotToken(value) {
		return fmt.Errorf("%s: not a GitHub token (want gho_/ghp_/ghu_/github_pat_, >=20 chars)", name)
	}
	return textutil.ValidCredential(name, value)
}

// StoreSecret seals a value using the vault's validation and name-only audit entry, without menu-specific checks.
func StoreSecret(name, value string) error {
	return vault.Store(name, value)
}

// DeleteSecret removes a sealed value and retains the vault's name-only audit entry.
func DeleteSecret(name string) error {
	return vault.Delete(name)
}

// SaveEnvEdit saves the secret before the env reference; the menu separately confirms and reports old-secret deletion.
func SaveEnvEdit(name, raw, secret string) error {
	if secret != "" {
		if err := StoreSecret(name, secret); err != nil {
			return err
		}
	}
	return providers.SetCustomEnv(name, raw)
}
