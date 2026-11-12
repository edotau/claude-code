package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
)

// Seams for tests.
var (
	findClaude = ClaudeBinary
	selfBinary = paths.SelfBinary
)

const defaultAnthropicBase = "https://api.anthropic.com"

// oauthKey survives the wipe only for cli-auth (subscription login) profiles.
const oauthKey = "CLAUDE_CODE_OAUTH_TOKEN"

// dropLaunchOwned is launch.env minus the names the launch wipes and resolves itself, so it never redirects the route.
func dropLaunchOwned(env []string) []string {
	return slices.DeleteFunc(env, func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return k == oauthKey || slices.Contains(providers.WipeKeys, k)
	})
}

// claudePlan resolves a Claude Code launch.
func claudePlan(ctx context.Context, o agent.LaunchOptions, args []string) (agent.Plan, error) {
	reg, sel, err := agent.SessionSelection(o, true)
	if err != nil {
		return agent.Plan{}, err
	}
	bin, self, err := claudeLaunchBinaries()
	if err != nil {
		return agent.Plan{}, err
	}
	p := sel.Provider
	passthrough := p.Auth.Method() == providers.AuthCLI
	if err := refuseProvider(sel); err != nil {
		return agent.Plan{}, err
	}
	plan, err := newClaudePlan(bin, o, sel, passthrough)
	if err != nil {
		return agent.Plan{}, err
	}
	offerLogin(ctx, o, args, sessionProviders(sel), &plan)
	proxied := sel.NeedsRouter(reg) || armedHop(p, args)
	helper, settings, err := configureRouting(ctx, sel, p, passthrough, proxied, self, &plan)
	if err != nil {
		return agent.Plan{}, err
	}
	if err := applyOverlay(&plan, passthrough, args, helper, settings, p.Env); err != nil {
		return agent.Plan{}, err
	}
	if !agent.IsHeadless(args) { // a headless launch renders in claudeCmd, where the request is known
		if err := renderInteractive(ctx, &plan, providers.Target{Provider: p, Model: o.Model}, args); err != nil {
			return agent.Plan{}, err
		}
	}
	plan.Argv = append(plan.Argv, args...)
	plan.Keep = o.KeepEnv
	keepShellEndpoint(&plan, o)
	return plan, nil
}

// newClaudePlan is the plan before routing: the wiped keys, the model and thinking knobs, and the session profile.
func newClaudePlan(bin string, o agent.LaunchOptions, sel providers.Selection, passthrough bool) (agent.Plan, error) {
	p := sel.Provider
	plan := agent.Plan{Binary: bin, Set: map[string]string{}, Unset: append([]string(nil), providers.WipeKeys...)}
	if !passthrough {
		plan.Unset = append(plan.Unset, oauthKey)
	}
	if configDirEnvIsDefault() {
		plan.Unset = append(plan.Unset, proc.EnvClaudeConfigDir)
	}
	setModelAliasEnv(&plan, sel)
	if err := setMainModel(&plan, sel, p, agent.MainAlias(o), o.Model != "" || vault.GetAgentRecord("claude").Model != ""); err != nil {
		return agent.Plan{}, err
	}
	set1MOff(&plan, sel, p)
	plan.Set[providers.EnvSessionProfile] = p.Name
	return plan, nil
}

// refuseProvider refuses a vendor sign-in and any AI Gateway-wire row; only the proxy keeps that wire.
func refuseProvider(sel providers.Selection) error {
	if p := sel.Provider; p.Auth.Login != "" {
		return fmt.Errorf("profile %s is the %s sign-in and cannot back Claude Code; use claude-code %s --profile %s", p.Name, p.Auth.Login, p.Auth.Login, p.Name)
	}
	for _, p := range sessionProviders(sel) {
		if p.IsAIGateway() {
			return fmt.Errorf("profile %s uses the AI Gateway wire; agents run on /serving-endpoints only", p.Name)
		}
	}
	return nil
}

// renderInteractive adds the record's effort (unless args type one) and launch.env/launch.args; route keys win.
func renderInteractive(ctx context.Context, plan *agent.Plan, t providers.Target, args []string) error {
	st, err := agent.ResolveSettings(agentid.Claude, agent.RunOptions{}, t, false)
	if st.Agent == "" { // not even the built-in settings resolved
		return fmt.Errorf("claude settings: %w", err)
	}
	if err != nil { // a refused stored record falls back to the built-in settings; the launch says so
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("claude stored agent settings not applied: %v; built-in defaults used", err))
	}
	if typesEffort(args) {
		st.Effort = ""
	}
	env, argv, _, _, err := claudeAgent.RenderSettings(ctx, st, nil, nil) // the cleanup is the base's no-op
	if err != nil {
		return err
	}
	for _, kv := range dropLaunchOwned(env) {
		k, v, _ := strings.Cut(kv, "=")
		if _, set := plan.Set[k]; !set {
			plan.Set[k] = v
		}
	}
	plan.Argv = append(plan.Argv, argv...)
	return nil
}

// typesEffort reports a --effort flag in args before any "--", where the prompt starts.
func typesEffort(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "--effort" || strings.HasPrefix(a, "--effort=") {
			return true
		}
	}
	return false
}

// keepShellEndpoint lets a shell ANTHROPIC_MODEL (no --model flag) or ANTHROPIC_BASE_URL win over the vault resolution;
// credentials stay scrubbed.
func keepShellEndpoint(plan *agent.Plan, o agent.LaunchOptions) {
	keep := []string{proc.EnvAnthropicBaseURL}
	if o.Model == "" {
		keep = append(keep, "ANTHROPIC_MODEL")
	}
	own := plan.Set[proc.EnvAnthropicBaseURL]
	if own == "" {
		own = defaultAnthropicBase // an unset plan base means the default route
	}
	base := strings.TrimSpace(os.Getenv(proc.EnvAnthropicBaseURL))
	inherited := inheritedBase(base, own)
	if base != "" && !inherited && !sameEndpoint(base, own) {
		plan.ForeignBase = true
		dropHarnessCredential(plan)
	}
	keepShellKeys(plan, keep, inherited)
}

func keepShellKeys(plan *agent.Plan, keys []string, inherited bool) {
	for _, k := range keys {
		if strings.TrimSpace(os.Getenv(k)) == "" || (k == proc.EnvAnthropicBaseURL && inherited) {
			continue
		}
		delete(plan.Set, k)
		plan.Unset = slices.DeleteFunc(plan.Unset, func(u string) bool { return u == k })
		plan.Keep = append(slices.Clone(plan.Keep), k)
	}
}

// sessionProxyBase reports raw is the proxy base this harness session was launched on (/p/<CLAUDE_PROFILE>).
func sessionProxyBase(raw string) bool {
	u, err := url.Parse(raw)
	sp := providers.SessionProfile()
	if err != nil || sp == "" || strings.TrimRight(u.Path, "/") != "/p/"+sp {
		return false
	}
	st, err := readProxyState() // a /p/<profile> base on another port is the user's own override, not this session's
	return err == nil && st.Port != 0 && u.Port() == strconv.Itoa(st.Port)
}

// inheritedBase: the shell base is this session's (proxy, /p/<profile>, direct route); a hop drops it, keeps its login.
func inheritedBase(base, own string) bool {
	if isProxySessionBase(base) && (isProxySessionBase(own) && sameHostPort(base, own) || sessionProxyBase(base)) {
		return true
	}
	return sessionRouteBase(base)
}

// sessionRouteBase reports raw is the session profile's own Anthropic route: a direct, unproxied Databricks session.
func sessionRouteBase(raw string) bool {
	sp := providers.SessionProfile()
	if raw == "" || sp == "" {
		return false
	}
	reg, err := providers.Load()
	if err != nil {
		return false
	}
	p, err := reg.Get(sp)
	if err != nil {
		return false
	}
	route, ok := p.Route(providers.ProtocolAnthropic)
	return ok && sameEndpoint(raw, route)
}

// readProxyState is the live proxy's state; tests swap it so no real daemon is read.
var readProxyState = gateway.ReadState

// isProxySessionBase reports whether raw is a loopback proxy session base (http://127.0.0.1:<port>/p/<profile>).
func isProxySessionBase(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !strings.HasPrefix(u.Path, "/p/") {
		return false
	}
	return textutil.IsLoopbackHost(u.Hostname())
}

// sameHostPort reports whether a and b name the same host:port, so a loopback proxy on another port is not ours.
func sameHostPort(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	return errA == nil && errB == nil && strings.EqualFold(ua.Host, ub.Host)
}

// sameEndpoint compares two base URLs by scheme, host and path, ignoring case in the host and a trailing slash.
func sameEndpoint(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host) &&
		strings.TrimRight(ua.Path, "/") == strings.TrimRight(ub.Path, "/")
}

// dropKeyHelper removes the apiKeyHelper overlay: its TTL var, the --settings pair and the overlay file.
func dropKeyHelper(plan *agent.Plan) {
	if len(plan.Files) > 0 && len(plan.Argv) >= 2 && plan.Argv[0] == "--settings" && plan.Argv[1] == plan.Files[0].Path {
		plan.Argv = slices.Clone(plan.Argv[2:])
	}
	plan.Files = nil
	delete(plan.Set, "CLAUDE_CODE_API_KEY_HELPER_TTL_MS")
}

// dropHarnessCredential strips the apiKeyHelper, headers and setup-token so a foreign endpoint never gets them.
func dropHarnessCredential(plan *agent.Plan) {
	dropKeyHelper(plan)
	for _, k := range []string{proc.EnvAnthropicCustomHeaders, oauthKey} {
		delete(plan.Set, k)
	}
	if !slices.Contains(plan.Keep, oauthKey) { // an inherited token must leave the child env too, not only the plan
		plan.Unset = append(plan.Unset, oauthKey)
	}
	plan.Warnings = append(plan.Warnings, "shell ANTHROPIC_BASE_URL kept: the profile credential is withheld; export HARNESS_KEEP_ENV=<name> to pass your own")
}

// configDirEnvIsDefault: an explicit CLAUDE_CONFIG_DIR=~/.claude makes claude write a stray ~/.claude/.claude.json.
func configDirEnvIsDefault() bool {
	return strings.TrimSpace(os.Getenv(proc.EnvClaudeConfigDir)) != "" && paths.ConfigDirIsDefault()
}

// setModelAliasEnv stamps ANTHROPIC_DEFAULT_<SLOT>_MODEL for every alias the selection resolved.
func setModelAliasEnv(plan *agent.Plan, sel providers.Selection) {
	for _, alias := range models.AliasNames {
		if m := sel.ClientModel(alias); m != "" {
			plan.Set["ANTHROPIC_DEFAULT_"+strings.ToUpper(alias)+"_MODEL"] = m
		}
	}
	if m := sel.ClientModel(models.Opus); m != "" { // classifier default claude-sonnet-5[1m] is unserved off Anthropic
		plan.Set["CLAUDE_CODE_AUTO_MODE_MODEL"] = m
	}
}

// set1MOff turns 1M off when the session endpoint lacks it: Claude Code assumes 1M for recognised ids. An explicit
// [1m] typed on an alias keeps it on. DISABLE_1M_CONTEXT over AUTO_COMPACT_WINDOW: it also drops the 1M /model entries.
func set1MOff(plan *agent.Plan, sel providers.Selection, p *providers.Provider) {
	if p.Serves1M() {
		return
	}
	for _, t := range sel.Aliases {
		if t.OneM {
			return
		}
	}
	plan.Set["CLAUDE_CODE_DISABLE_1M_CONTEXT"] = "1"
}

// setMainModel sets the thinking knobs from alias's model, and ANTHROPIC_MODEL only when the user asked for one.
func setMainModel(plan *agent.Plan, sel providers.Selection, p *providers.Provider, alias string, asked bool) error {
	opus := sel.ClientModel(alias)
	if opus == "" {
		return fmt.Errorf("profile %s has no %s model; pin one with claude-code models --pin %s=<id>", p.Name, alias, alias)
	}
	if asked {
		plan.Set["ANTHROPIC_MODEL"] = opus
	}
	if maxTok, disable := models.ThinkingKnobs(opus); maxTok != "" {
		plan.Set["MAX_THINKING_TOKENS"] = maxTok
		if disable != "" {
			plan.Set["CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING"] = disable
		}
	}
	return nil
}

// sessionProviders is the session provider plus each alias's provider.
func sessionProviders(sel providers.Selection) []*providers.Provider {
	out := []*providers.Provider{sel.Provider}
	for _, t := range sel.Aliases {
		out = append(out, t.Provider)
	}
	return out
}

// armedHop: a headless launch on the armed profile uses /p/<p> even when NeedsRouter would leave it direct.
func armedHop(p *providers.Provider, args []string) bool {
	arm := router.ArmProfile()
	return arm != "" && p.Name == arm && agent.IsHeadless(args)
}

// configureRouting wires the router base URL/headers when proxied, else the provider's own route, and
// returns the apiKeyHelper command for the overlay.
func configureRouting(ctx context.Context, sel providers.Selection, p *providers.Provider, passthrough, proxied bool, self string, plan *agent.Plan) (helper, settings string, err error) {
	for _, t := range sel.Aliases {
		if err := providers.ClaudeCodeError(t.Provider, t.Model); err != nil {
			return "", "", err
		}
	}
	if proxied {
		return configureClaudeProxy(ctx, sel, p, passthrough, self, plan)
	}
	helper = textutil.QuoteSH(helperBin(self)) + " resolve --profile " + textutil.QuoteSH(p.Name)
	if id := providers.IdentityFrom(ctx); id != "" {
		helper += " --as " + textutil.QuoteSH(id)
	}
	if route, ok := p.Route(providers.ProtocolAnthropic); ok && route != defaultAnthropicBase {
		plan.Set[proc.EnvAnthropicBaseURL] = route
	}
	if h := providers.HeaderLines(p.Headers); h != "" {
		plan.Set[proc.EnvAnthropicCustomHeaders] = h
	}
	if !providers.CredentialPresent(p) {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: no credential found yet (%s); the apiKeyHelper will fail until one is set", p.Name, p.Auth.Env))
	}
	return helper, filepath.Join(paths.StateDir(), "overlay-"+p.Name+".json"), nil
}

func configureClaudeProxy(ctx context.Context, sel providers.Selection, p *providers.Provider, passthrough bool, self string, plan *agent.Plan) (string, string, error) {
	if err := refuseCLIAliases(sel, passthrough); err != nil {
		return "", "", err
	}
	base, err := agent.EnsureRouter(ctx)
	if err != nil {
		return "", "", fmt.Errorf("proxy: %w", err)
	}
	plan.Set[proc.EnvAnthropicBaseURL] = gateway.SessionBase(base, p.Name)
	hdrs := []string{gateway.AgentHeader + ": claude"} // Claude Code adds the session id itself.
	if id := providers.IdentityFrom(ctx); id != "" {
		hdrs = append(hdrs, gateway.IdentityHeader+": "+id)
	}
	tok, err := armToken(ctx, base, p.Name)
	if err != nil {
		return "", "", err
	}
	if tok != "" {
		plan.Set[ProxyKeyEnv] = tok
	}
	if passthrough {
		secret, err := clientSecret(tok)
		if err != nil {
			return "", "", err
		}
		hdrs = append(hdrs, gateway.ClientHeader+": "+secret)
	}
	plan.Set[proc.EnvAnthropicCustomHeaders] = strings.Join(hdrs, "\n")
	return textutil.QuoteSH(helperBin(self)) + " resolve --proxy", filepath.Join(paths.StateDir(), "overlay-"+p.Name+"-proxy.json"), nil
}

// refuseCLIAliases refuses an alias on a cli login unless the session profile is a cli-login one too.
func refuseCLIAliases(sel providers.Selection, passthrough bool) error {
	for _, t := range sel.Aliases {
		if t.Provider.Auth.Method() == providers.AuthCLI && !passthrough {
			return fmt.Errorf("model on %s: its cli login needs a cli-login session profile — claude-code switch %s, or pick a key profile", t.Provider.Name, t.Provider.Name)
		}
	}
	return nil
}

// clientSecret is the client header's router secret (Authorization holds the login): token tok, else the gateway's.
func clientSecret(tok string) (string, error) {
	if tok != "" {
		return tok, nil
	}
	secret, err := gateway.ClientSecret()
	if err != nil {
		return "", fmt.Errorf("proxy secret: %w", err)
	}
	return secret, nil
}

// ProxyKeyEnv carries an armed launch's session token to `resolve --proxy`; ScrubDeny's HARNESS_*_API_KEY drops it
// from grandchildren, so a nested launch mints its own.
const ProxyKeyEnv = "HARNESS_PROXY_API_KEY"

// armToken is this process's session token for profile on routerBase inside overlay databricks:<p>, minted once;
// "" when not armed, so the child keeps the router secret.
func armToken(ctx context.Context, routerBase, profile string) (string, error) {
	if router.ArmProfile() == "" {
		return "", nil
	}
	tok, err := agent.SessionToken(ctx, routerBase, profile)
	if err != nil {
		return "", fmt.Errorf("proxy: session token for profile %s: %w", profile, err)
	}
	return tok, nil
}

func claudeLaunchBinaries() (string, string, error) {
	bin, err := findClaude()
	if err != nil {
		return "", "", err
	}
	self, err := selfBinary()
	return bin, self, err
}

// applyOverlay sets the passthrough OAuth/unset handling, or the apiKeyHelper overlay + --settings argv.
func applyOverlay(plan *agent.Plan, passthrough bool, args []string, helper, settings string, env map[string]string) error {
	// Provider env (non-secret by Validate) rides the child's process env: Claude Code reads it before settings load.
	for k, v := range providerEnv(env) {
		if _, set := plan.Set[k]; !set { // the harness-resolved key wins
			plan.Set[k] = v
		}
	}
	if passthrough { // no overlay: Claude Code disables subscription OAuth whenever an apiKeyHelper is set
		if !agent.IsHeadless(args) {
			// Remote Control refuses the inference-only setup-token; the interactive /login credential must win.
			plan.Unset = append(plan.Unset, oauthKey)
		} else if os.Getenv(oauthKey) == "" {
			// A long-lived setup-token from the env or the vault outlives the /login session in .credentials.json.
			if tok := providers.HeadlessOAuthToken(); tok != "" {
				plan.Set[oauthKey] = tok
			}
		}
		return nil
	}
	plan.Set["CLAUDE_CODE_API_KEY_HELPER_TTL_MS"] = "300000"
	overlay, err := json.MarshalIndent(map[string]string{"apiKeyHelper": helper}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings overlay: %w", err)
	}
	plan.Files = []agent.LaunchFile{{Path: settings, Data: overlay}}
	plan.Argv = []string{"--settings", settings}
	return nil
}

// providerEnv is the provider's non-secret Env minus keys the process env or the vault custom env already set.
func providerEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	custom := providers.CustomEnv()
	out := map[string]string{}
	for k, v := range env {
		if _, ok := custom[k]; ok || os.Getenv(k) != "" {
			continue
		}
		out[k] = v
	}
	return out
}

// ClaudeBinary finds claude: BinDir/claude, else the native installer's newest (not under the bin-dir override).
func ClaudeBinary() (string, error) {
	bin, err := paths.AgentBin("claude")
	if err == nil {
		return bin, nil
	}
	if os.Getenv(paths.AgentBinDirEnv) != "" {
		return "", err // the override is hermetic: never fall back to the real install
	}
	if bin := newestInstalledClaude(); bin != "" {
		return bin, nil
	}
	return "", errors.New("claude: not found in " + paths.BinDir() +
		" or ~/.local/share/claude/versions (claude-code install --tools=claude)")
}

func newestInstalledClaude() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".local", "share", "claude", "versions")
	entries, _ := os.ReadDir(dir)
	self, _ := paths.SelfBinary()
	best, newest := "", "0"
	for _, e := range entries {
		cand := filepath.Join(dir, e.Name())
		fi, err := os.Stat(cand)
		if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			continue
		}
		if real, _ := filepath.EvalSymlinks(cand); real == self {
			continue
		}
		if c, ok := textutil.CompareVersions(newest, e.Name()); ok && c < 0 { // a non-numeric folder name is skipped
			best, newest = cand, e.Name()
		}
	}
	return best
}

// helperBin prefers the installed binary: the overlay file is shared, so a scratch build must never be baked in.
func helperBin(self string) string {
	if bin := filepath.Join(paths.BinDir(), "claude-code"); paths.IsFile(bin) {
		return bin
	}
	return self
}

// Launch launches Claude Code with alias as the main-loop model ("" = opus) and returns the exit code.
func Launch(alias string, args []string) int {
	as, args, err := providers.TakeAs(args)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	opts, rest, err := agent.ParseLaunchArgs(args, true)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	if bareHelp(rest) { // claude's own help needs no profile or model, so no lookup may refuse it
		bin, err := ClaudeBinary()
		if err != nil {
			return helptext.Fail("%v", err)
		}
		return helptext.WrappedDocs(bin)
	}
	opts.Alias = alias
	plan, err := claudePlan(providers.WithIdentity(context.Background(), as), opts, rest)
	if err == nil && !opts.PrintEnv && !agent.IsHeadless(rest) {
		providers.RecordLastProfile("claude", plan.Set[providers.EnvSessionProfile])
	}
	return agent.RunPlan("claude", plan, opts, err)
}

// bareHelp is a lone help flag; `claude mcp --help` still goes to claude so the subcommand's help is kept.
func bareHelp(rest []string) bool { return len(rest) == 1 && helptext.WantsHelp(rest) }

// LaunchScience is `anthropic science`: only a leading --print-env; every other flag is claude-science's own.
func LaunchScience(args []string) int {
	var opts agent.LaunchOptions
	if len(args) > 0 && args[0] == "--print-env" {
		opts.PrintEnv, args = true, args[1:]
	}
	plan, err := scienceEnv(args)
	return agent.RunPlan("claude-science", plan, opts, err)
}

// LaunchAnthropic is first-party Claude Code with a login, OAuth token or API key; vaultSet stores a missing secret.
func LaunchAnthropic(args []string, vaultSet func(name string) int) int {
	if err := helptext.RemovedFlagHint(args); err != nil {
		return helptext.Fail("%v", err)
	}
	o, rest, err := parseAnthropicArgs(args)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	if o.Provider != "" { // a named profile launches like `claude --profile P`, on that profile's credential
		return Launch("", args)
	}
	secret := ""
	if o.Mode != authLogin {
		if secret, err = anthropicSecret(o, vaultSet); err != nil {
			return helptext.Fail("%v", err)
		}
	}
	plan, err := anthropicPlan(context.Background(), o, rest, secret)
	return agent.RunPlan("claude", plan, o.LaunchOptions, err)
}
