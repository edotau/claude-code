package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/network/egress"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// PinnedProfile is agent name's vault-pinned registry profile; "" if unpinned or off, an error if the pin names no row.
func PinnedProfile(name string) (string, error) {
	pins, err := AgentPins()
	if err != nil {
		return "", err
	}
	pin, ok := pins[name]
	if !ok || pin.Off || pin.Profile == "" {
		return "", nil
	}
	reg, err := providers.Load()
	if err != nil {
		return "", err
	}
	if err := ValidAgentPin(reg, name, pin.Profile); err != nil {
		return "", fmt.Errorf("pinned profile: %w", err)
	}
	if reg.Providers[pin.Profile] == nil {
		return "", nil // an own-login or PinIDs pin (opencode-go) launches on the agent's own plan
	}
	return pin.Profile, nil
}

// Configure returns extra env (KEY=VALUE) and leading argv that point the agent CLI at target.
func (s Schema) Configure(ctx context.Context, target providers.Target) (env, argv []string, err error) {
	return s.configure(ctx, target, nil)
}

// ConfigureHeadless is Configure's agent form with the per-run options each CLI can carry.
func (s Schema) ConfigureHeadless(ctx context.Context, target providers.Target, h Headless) (env, argv []string, err error) {
	if err := CheckPrompt(h); err != nil {
		return nil, nil, err
	}
	return s.configure(ctx, target, &h)
}

// configure resolves the route, then renders the settings on every route; a headless h renders in cliEngine.prepare.
func (s Schema) configure(ctx context.Context, t providers.Target, h *Headless) ([]string, []string, error) {
	env, argv, err := s.configureRoute(ctx, t, h)
	if err != nil || h != nil {
		return env, argv, err
	}
	// Route keys go last and shadow same-named launch.env entries, so a spec overlay can never redirect an agent.
	env, argv, _, _, err = s.Eng().RenderSettings(ctx, s.launchSettings(t, false), env, argv)
	return env, argv, err
}

// launchSettings is Settings for a launch that goes on anyway: a bad stored record is a notice, the built-ins apply.
func (s Schema) launchSettings(t providers.Target, headless bool) Settings {
	st, err := ResolveSettings(s.Name, RunOptions{}, t, headless)
	if err == nil {
		return st
	}
	if st.Agent == "" { // not even the built-in record loaded: nothing is applied
		fmt.Fprintf(NoticeOut, "claude-code: %s agent settings unreadable: %v; not applied\n", s.Name, err)
		return Settings{Agent: s.Name, Target: t, Headless: headless}
	}
	fmt.Fprintf(NoticeOut, "claude-code: %s stored agent settings not applied: %v; built-in defaults used\n", s.Name, err)
	return st
}

// RouteEnvLast is launchEnv minus any key the route env sets, then the route env: the route always wins.
func RouteEnvLast(launchEnv, env []string) []string {
	routeKeys := map[string]bool{}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		routeKeys[k] = true
	}
	var out []string
	for _, kv := range launchEnv {
		if k, _, _ := strings.Cut(kv, "="); !routeKeys[k] {
			out = append(out, kv)
		}
	}
	return append(out, env...)
}

// LaunchSpec is the agent's spec env and argv prefix for an own-login launch; a broken overlay never breaks it.
func (s Schema) LaunchSpec() (env, argv []string) {
	st := s.launchSettings(providers.Target{}, false)
	env, argv, _, _, _ = s.Eng().RenderSettings(context.Background(), st, nil, nil) // the default never errs
	return env, argv
}

func (s Schema) configureRoute(ctx context.Context, t providers.Target, h *Headless) ([]string, []string, error) {
	name := s.Name
	e, ok := driverFor(name)
	if !ok {
		return nil, nil, fmt.Errorf("unknown agent %q (%s)", name, strings.Join(Names(), "|"))
	}
	p := t.Provider
	if p == nil || t.Model == "" {
		return nil, nil, fmt.Errorf("%s: no provider/model resolved", name)
	}
	if p.IsAIGateway() {
		return nil, nil, fmt.Errorf("profile %s uses the AI Gateway wire; agents run on /serving-endpoints only", p.Name)
	}
	if p.Auth.Login != "" {
		return login(e, p, models.Strip1M(t.Model), h)
	}
	if len(s.CLI.Dialects) == 0 && GeminiGatewayRow(p) { // a CLI with no dialect reaches a gateway row only here
		return e.Wire(ctx, WireRoute{Target: t, Provider: p}, h)
	}
	if ok, why := Serves(name, p, ""); !ok {
		return nil, nil, fmt.Errorf("%s: %s", name, why)
	}
	return s.configureWire(ctx, e, t, h)
}

func (s Schema) configureWire(ctx context.Context, e Driver, t providers.Target, h *Headless) ([]string, []string, error) {
	p := t.Provider
	dialect, base := s.firstRoute(p)
	model := p.WireModel(models.Strip1M(t.Model)) // idempotent: targets not built by ResolveAgent get the wire id too
	var cred string
	var err error
	routed := false
	if p.Auth.Method() != providers.AuthNone && !IsDirect(ctx) {
		// Keyed provider: the agent gets the router base + router secret; the real credential stays in the router.
		if p, base, cred, err = routerRoute(ctx, s, p, dialect); err != nil {
			return nil, nil, err
		}
		routed = true
	} else if cred, err = providers.Credential(ctx, p); err != nil {
		return nil, nil, err
	}
	return e.Wire(ctx, WireRoute{Target: t, Provider: p, Dialect: dialect, Base: base, Model: model, Cred: cred, Routed: routed}, h)
}

func (s Schema) firstRoute(p *providers.Provider) (string, string) {
	for _, d := range s.CLI.Dialects {
		if base, ok := p.Route(d); ok {
			return d, base
		}
	}
	return "", ""
}

// Router seams (tests stub these so no daemon is spawned); EnsureRouter starts or finds the loopback router.
var (
	EnsureRouter = ensureRouter
	RouterSecret = gateway.ClientSecret
	MintSession  = gateway.MintSessionToken // mints a router session token for profile and pid
)

// ensureRouter is gateway.Ensure; under --remote it brings the tunnel up here and returns the /via/<host> base.
func ensureRouter(ctx context.Context) (string, error) {
	base, err := gateway.Ensure(ctx)
	if err != nil || egress.Host() == "" {
		return base, err
	}
	if _, err := egress.Ensure(ctx, egress.Host()); err != nil {
		return "", err
	}
	return gateway.ViaBase(base), nil
}

// refuseDirect rejects --direct under --remote: a direct agent calls the provider itself, around the tunnel.
func refuseDirect(direct bool) error {
	if direct && egress.Host() != "" {
		return fmt.Errorf("--direct cannot be combined with --remote %s: a direct agent would bypass the egress host", egress.Host())
	}
	return nil
}

type directKey struct{}

// WithDirect makes configure skip the router and hand the agent the provider's own route and credential.
func WithDirect(ctx context.Context) context.Context {
	return context.WithValue(ctx, directKey{}, true)
}

func IsDirect(ctx context.Context) bool { v, _ := ctx.Value(directKey{}).(bool); return v }

// routerRoute ensures the router daemon and returns a copy of p routed through it with the router secret as key.
func routerRoute(ctx context.Context, s Schema, p *providers.Provider, dialect string) (*providers.Provider, string, string, error) {
	rb, err := EnsureRouter(ctx)
	if err != nil {
		return nil, "", "", fmt.Errorf("%s: router: %w", s.Name, err)
	}
	secret, err := routerKey(ctx, rb, p.Name)
	if err != nil {
		return nil, "", "", fmt.Errorf("%s: %w", s.Name, err)
	}
	base := gateway.SessionBase(rb, p.Name) + s.CLI.RouterSuffix[dialect]
	rp := *p
	rp.Headers = map[string]string{gateway.AgentHeader: s.Name} // the router adds the provider's own headers upstream
	if id := providers.IdentityFrom(ctx); id != "" {
		rp.Headers[gateway.IdentityHeader] = id
	}
	return &rp, base, secret, nil
}

// routerKey is the key a routed child holds: the router secret, or inside databricks:<p> a session token for profile.
func routerKey(ctx context.Context, routerBase, profile string) (string, error) {
	if router.ArmProfile() == "" {
		secret, err := RouterSecret()
		if err != nil {
			return "", fmt.Errorf("router secret: %w", err)
		}
		return secret, nil
	}
	tok, err := SessionToken(ctx, routerBase, profile)
	if err != nil {
		return "", fmt.Errorf("session token for profile %s: %w", profile, err)
	}
	return tok, nil
}

// sessionToken is one router+profile token slot; its own lock keeps a slow mint from stalling other profiles.
type sessionToken struct {
	mu     sync.Mutex
	tok    string
	minted time.Time
}

// sessionTokenMaxAge re-mints well inside the router's 12h idle lifetime: a long-lived process never holds a dead one.
const sessionTokenMaxAge = time.Hour

// sessionTokens holds one session token per router base and profile, bound to this pid (it dies with the process).
var (
	sessionMu     sync.Mutex
	sessionTokens = map[string]*sessionToken{}
)

// SessionToken mints profile's token once per router (batch hops share it), so retries never fill the token cap.
func SessionToken(ctx context.Context, routerBase, profile string) (string, error) {
	sessionMu.Lock()
	e := sessionTokens[routerBase+"|"+profile]
	if e == nil {
		e = &sessionToken{}
		sessionTokens[routerBase+"|"+profile] = e
	}
	sessionMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.tok != "" && time.Since(e.minted) < sessionTokenMaxAge {
		return e.tok, nil
	}
	tok, err := MintSession(ctx, profile, os.Getpid())
	if err != nil {
		return "", err
	}
	e.tok, e.minted = tok, time.Now()
	return tok, nil
}

// ResetSessionTokens forgets every cached session token; a test that swaps MintSession calls it.
func ResetSessionTokens() {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	clear(sessionTokens)
}

// login runs the agent on its own subscription sign-in; a login provider serves only the CLI it names.
func login(e Driver, p *providers.Provider, model string, h *Headless) ([]string, []string, error) {
	if !e.LoginServes(p) {
		return nil, nil, fmt.Errorf("%s: provider %s is the %s sign-in; use claude-code %s --profile %s", e.Name(), p.Name, p.Auth.Login, p.Auth.Login, p.Name)
	}
	return e.Login(p, model, h)
}

// StubRouter points configure at base (unstubbed Ensure would relaunch the test binary as the daemon).
func StubRouter(base string) (restore func()) {
	oe := EnsureRouter
	EnsureRouter = func(context.Context) (string, error) { return base, nil }
	return func() { EnsureRouter = oe }
}

// StubProxyForTest stubs the router, its secret and session mints for a test binary (call from TestMain).
func StubProxyForTest(base, secret string) {
	EnsureRouter = func(context.Context) (string, error) { return base, nil }
	RouterSecret = func() (string, error) { return secret, nil }
	MintSession = func(context.Context, string, int) (string, error) { return "", errors.New("session mint is stubbed") }
	retry.SharedPoll = 25 * time.Millisecond
}

// NoticeOut is where launch and agent-fallback notices print; tests discard it (also via other packages' TestMain).
var NoticeOut io.Writer = os.Stderr

// ClaudeLaunch is a resolved Claude Code launch: the binary, its argv and env, and the route keys the plan set.
type ClaudeLaunch struct {
	Binary    string
	Argv, Env []string
	Set       map[string]string
}

// ClaudeOptions picks the session route for a Claude launch.
type ClaudeOptions struct {
	Provider, Model string
	KeepEnv         []string
}

// Launcher resolves a Claude launch; the claude package registers the default and tests fake it.
type Launcher interface {
	Claude(ctx context.Context, o ClaudeOptions, args []string) (ClaudeLaunch, error)
}

// defaultLauncher is the Launcher the one Register that supplies it installs; the Client hands it to new runs.
var defaultLauncher Launcher

// LaunchOptions are the harness flags consumed before passthrough.
type LaunchOptions struct {
	Provider string // the profile name from --profile (field name kept until the agent lane moves off it)
	Model    string // alias, canonical version or wire id, this launch only
	Alias    string // opus|sonnet|haiku|fable: the main-loop model; the other slots keep their own pins
	PrintEnv bool
	Direct   bool     // agents only: skip the proxy, hand the CLI the profile route
	KeepEnv  []string // names the credential scrub must not drop, from --keep-env
	File     string   // codex only: a config.toml whose keys ride as -c overrides after the harness's, from -f
	Overlay  string   // --overlay <value>; empty reads $COWORKER_OVERLAY
}

func (o *LaunchOptions) launchFlagSetter(name string, claude bool) func(string) {
	fields := map[string]*string{"--profile": &o.Provider, "--model": &o.Model, "--overlay": &o.Overlay}
	if !claude {
		fields["-f"], fields["--file"] = &o.File, &o.File
	}
	if field := fields[name]; field != nil {
		return func(v string) { *field = v }
	}
	if name == "--keep-env" {
		return func(v string) { o.KeepEnv = append(o.KeepEnv, strings.Split(v, ",")...) }
	}
	return nil
}

// LaunchFile is a 0600 per-launch config Exec writes before the child starts; a pid-named one is swept on exit.
type LaunchFile struct {
	Path string
	Data []byte
}

// Plan is a fully resolved launch: exec Binary with Argv under the base env minus Unset plus Set.
type Plan struct {
	Binary      string
	Agent       string // the launched agent, for RecordOverlayDebug at the exec site
	Argv        []string
	Set         map[string]string
	Unset       []string
	Keep        []string     // names the credential scrub must not drop, from LaunchOptions.KeepEnv
	CLI         bool         // true for an agent CLI child: HARNESS_VAULT_KEY is scrubbed too (never for Claude itself)
	Files       []LaunchFile // written 0600 by WriteFiles before the child starts
	Warnings    []string
	ForeignBase bool   // the shell's ANTHROPIC_BASE_URL differs from the profile's: no harness credential may go there
	Profile     string // the profile an agent CLI launch resolved, for RecordLastProfile
	OnExecFail  func() // runs when Exec returns (the exec failed): drops state minted for the launch
}

// Env applies the plan to base (KEY=VALUE list).
func (p Plan) Env(base []string) []string {
	return p.envWith(base, customEnv())
}

// customEnv is the vault env bucket (@secret refs via vault.Lookup); an unresolved ref is warned and left out.
var customEnv = func() map[string]string {
	errored := map[string]bool{}
	vals, missing, err := providers.ResolveCustomEnv(func(k string) (string, bool) {
		v, _, err := vault.Lookup(k)
		if err != nil {
			fmt.Fprintf(NoticeOut, "claude-code: custom env %s: %v; not set\n", k, err)
			errored[k] = true
			return "", false
		}
		return v, v != ""
	})
	if err != nil {
		fmt.Fprintf(NoticeOut, "claude-code: custom env unreadable: %v\n", err)
	}
	for _, k := range missing {
		if errored[k] {
			continue // already warned above with the real error
		}
		fmt.Fprintf(NoticeOut, "claude-code: custom env %s: its @secret is missing from the vault or not allowed (route or harness secret); not set\n", k)
	}
	return vals
}

// envWith layers base, then custom (user vars), then Set; Unset and Set beat custom.
func (p Plan) envWith(base []string, custom map[string]string) []string {
	unset := map[string]bool{}
	for _, k := range p.Unset {
		unset[k] = true
	}
	merged := maps.Clone(p.Set)
	if merged == nil {
		merged = map[string]string{}
	}
	for k, v := range custom {
		_, harness := p.Set[k]
		if !harness && !unset[k] {
			merged[k] = v
		}
	}
	p.Set = merged
	return p.plainEnv(base)
}

// envMode is the deny scrub this plan's child gets: agent CLIs also lose HARNESS_VAULT_KEY.
func (p Plan) envMode() EnvMode {
	if p.CLI {
		return EnvAgent
	}
	return EnvClaude
}

// plainEnv is the plan as an EnvSpec: base scrubbed, minus Unset and Set names, then Set in name order.
func (p Plan) plainEnv(base []string) []string {
	set := make([]string, 0, len(p.Set))
	for _, k := range slices.Sorted(maps.Keys(p.Set)) {
		set = append(set, k+"="+p.Set[k])
	}
	spec := EnvSpec{Mode: p.envMode(), Base: append([]string{}, base...), Keep: p.Keep, Unset: p.Unset, Set: set}
	return spec.Environ(context.Background()) // interactive: no batch isolation
}

// envRedactors blank the secrets inside a structured env value (key -> redactor), filled by Register.
var envRedactors = map[string]func(string) string{}

// Print writes the resolved launch; header values are masked since static headers may carry secrets.
func (p Plan) Print(w io.Writer) {
	fmt.Fprintf(w, "binary   %s\nargv     %s\n", p.Binary, redactQuoted(strings.Join(p.Argv, " ")))
	for _, k := range p.unsetNames() {
		fmt.Fprintf(w, "unset    %s\n", k)
	}
	for _, k := range slices.Sorted(maps.Keys(p.Set)) {
		v := p.Set[k]
		switch {
		case providers.IsSecretName(k):
			v = "<redacted>"
		case envRedactors[k] != nil:
			v = redactQuoted(envRedactors[k](v))
		}
		fmt.Fprintf(w, "set      %s=%s\n", k, v)
	}
	for _, k := range slices.Sorted(maps.Keys(providers.CustomEnv())) {
		switch _, harness := p.Set[k]; {
		case harness || slices.Contains(p.Unset, k):
			fmt.Fprintf(w, "custom   %s (overridden by the launch)\n", k)
		default:
			fmt.Fprintf(w, "custom   %s (vault env)\n", k)
		}
	}
	for _, f := range p.Files {
		fmt.Fprintf(w, "overlay  %s\n%s\n", f.Path, redactQuoted(string(f.Data)))
	}
	for _, warn := range p.Warnings {
		fmt.Fprintf(w, "warning  %s\n", warn)
	}
}

// unsetNames is every inherited var this plan drops: the Anthropic wipe list plus the credential scrub, minus its sets.
func (p Plan) unsetNames() []string {
	names := map[string]bool{}
	for _, k := range p.Unset {
		if _, set := p.Set[k]; !set && os.Getenv(k) != "" {
			names[k] = true
		}
	}
	_, dropped := p.envMode().scrub(os.Environ(), p.Keep)
	for _, k := range dropped {
		if _, set := p.Set[k]; !set {
			names[k] = true
		}
	}
	return slices.Sorted(maps.Keys(names))
}

// WriteFiles writes the per-launch files the argv and env name.
func (p Plan) WriteFiles() error {
	for _, f := range p.Files {
		if err := paths.AtomicWrite(f.Path, f.Data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// recordEnv is the env Exec hands the child, recorded to COWORKER_OVERLAY_DEBUG; --print-env calls it as a dry exec.
func (p Plan) recordEnv() []string {
	env := p.Env(os.Environ())
	agentconfig.RecordOverlayDebug(p.Agent, env)
	return env
}

// Exec writes the launch files and replaces this process with the target.
func (p Plan) Exec() error {
	if err := p.WriteFiles(); err != nil {
		p.execFailed()
		return err
	}
	for _, w := range p.Warnings {
		fmt.Fprintln(os.Stderr, "claude-code: warning:", w)
	}
	err := proc.Exec(p.Binary, p.Argv, p.recordEnv())
	p.execFailed()
	return err
}

func (p Plan) execFailed() {
	if p.OnExecFail != nil {
		p.OnExecFail()
	}
}

// quotedRE is one double-quoted string; codex -c TOML and opencode JSON carry static provider headers in them.
var quotedRE = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// redactQuoted masks every credential-shaped quoted string in s.
func redactQuoted(s string) string {
	return quotedRE.ReplaceAllStringFunc(s, func(q string) string {
		inner := q[1 : len(q)-1]
		if textutil.LooksSecret(inner) || textutil.RedactSecrets(inner) != inner {
			return `"<redacted>"`
		}
		return q
	})
}

var _ = schemas.Register(schemas.Format{
	Name: "settings-overlay", Owner: "internal/agent", Kind: schemas.Derived,
	Glob:    func() string { return filepath.Join(paths.StateDir(), "overlay-*.json") },
	Version: 1, Keep: "rewritten on every launch; Claude Code reads it, so no schema field",
})

// overlayIdle reports a launch the overlay changes nothing about: empty layer and no $COWORKER_OVERLAY to hide.
func overlayIdle(r agentconfig.Resolved) bool {
	return len(r.Set)+len(r.Unset)+len(r.Keep) == 0 && os.Getenv(agentconfig.OverlayVar) == ""
}

// CheckRouteNames refuses an overlay set or unset name that route (KEY=VALUE, before the overlay is appended) sets.
func CheckRouteNames(route, set, unset []string) error {
	own := map[string]bool{}
	for _, kv := range route {
		k, _, _ := strings.Cut(kv, "=")
		own[k] = true
	}
	for _, op := range []struct {
		verb  string
		names []string
	}{{"set", set}, {"unset", unset}} {
		for _, n := range op.names {
			if own[n] {
				return notStartedError{fmt.Errorf("overlay %s %s: set by the launch for its route; use --profile", op.verb, n)}
			}
		}
	}
	return nil
}

// notStartedError is a launcher refusal raised before any child process starts; the attempt marks it NotStarted.
type notStartedError struct{ error }

func (e notStartedError) Unwrap() error { return e.error }

// checkOverlay refuses an overlay that sets or unsets a name the plan's launch sets for its route.
func (p Plan) checkOverlay(r agentconfig.Resolved) error {
	for _, k := range slices.Sorted(maps.Keys(r.Set)) {
		if _, own := p.Set[k]; own {
			return fmt.Errorf("overlay set %s: set by the launch for its route; use --profile", k)
		}
	}
	for _, k := range r.Unset {
		if _, own := p.Set[k]; own {
			return fmt.Errorf("overlay unset %s: set by the launch for its route; use --profile", k)
		}
	}
	return nil
}

// ApplyOverlay layers r under req's --env (the shorthand wins); the child's $COWORKER_OVERLAY is the confinement only.
func (req RunOptions) ApplyOverlay(r agentconfig.Resolved) RunOptions {
	if overlayIdle(r) {
		return req
	}
	env := make([]string, 0, len(r.Set)+len(req.Env))
	for _, k := range slices.Sorted(maps.Keys(r.Set)) {
		env = append(env, k+"="+r.Set[k])
	}
	req.Env = append(env, req.Env...)
	req.KeepEnv = slices.Concat(req.KeepEnv, r.Keep)
	req.Unset = slices.Concat(req.Unset, r.Unset, []string{agentconfig.OverlayVar}) // Environ re-adds the confinement
	req.OverlaySet, req.OverlayUnset = slices.Sorted(maps.Keys(r.Set)), r.Unset
	return req
}

// confinementDoc is the inline overlay naming only this process's provider and profile (one hop); "": none.
func confinementDoc() string {
	provider, profile, _ := router.Coworker() // a bad value confines nothing here either
	return agentconfig.InlineOverlay(provider, profile)
}

// withOverlay layers r over an interactive plan like ApplyOverlay; the vault's standing document never reaches it.
func (p Plan) withOverlay(r agentconfig.Resolved) Plan {
	set := maps.Clone(r.Set)
	if set == nil {
		set = map[string]string{}
	}
	maps.Copy(set, p.Set)
	if doc := confinementDoc(); doc != "" {
		set[agentconfig.OverlayVar] = doc
	}
	p.Set, p.Keep = set, slices.Concat(p.Keep, r.Keep)
	unset := slices.DeleteFunc(slices.Clone(r.Unset), func(k string) bool { _, own := p.Set[k]; return own })
	p.Unset = slices.Concat(p.Unset, unset, []string{agentconfig.OverlayVar})
	return p
}
