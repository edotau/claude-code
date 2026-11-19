package claude

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
	"github.com/edotau/claude-code/internal/settings"
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

const oauthKey = "CLAUDE_CODE_OAUTH_TOKEN"

// dropLaunchOwned is launch.env minus the names the launch wipes and resolves itself, so it never redirects the route.
func dropLaunchOwned(env []string) []string {
	return slices.DeleteFunc(env, func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return k == oauthKey || slices.Contains(providers.WipeKeys, k)
	})
}

func claudePlan(ctx context.Context, o agent.LaunchOptions, args []string) (agent.Plan, error) {
	reg, sel, err := agent.SessionSelection(o, true)
	if err != nil {
		return agent.Plan{}, err
	}
	bin, self, err := claudeLaunchBinaries(o)
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
	rows, extra := modelPickerRows(reg, p, args)
	proxied := len(rows) > 0 || sel.NeedsRouter(reg) || armedHop(ctx, p, args)
	helper, overlayPath, err := configureRouting(ctx, sel, p, passthrough, proxied, self, &plan, extra...)
	if err != nil {
		return agent.Plan{}, err
	}
	if proxied && !agent.IsHeadless(args) {
		plan.Warnings = append(plan.Warnings, "Remote Control is unavailable through the proxy; launch a direct login session to keep it")
	}
	addr := proxyAddr(&plan, proxied)
	setAliasDescriptions(ctx, &plan, sel, addr)
	for i := range rows {
		rows[i].Description = clip80(rows[i].route + routeSuffix(addr))
	}
	if err := applyOverlay(ctx, &plan, passthrough, args, helper, overlayPath, p, rows); err != nil {
		return agent.Plan{}, err
	}
	if !agent.IsHeadless(args) { // a headless launch renders in claudeCmd, where the request is known
		if err := renderInteractive(ctx, &plan, providers.Target{Provider: p, Model: o.Model}, args); err != nil {
			return agent.Plan{}, err
		}
	}
	plan.Argv = append(plan.Argv, args...)
	plan.Keep = o.KeepEnv
	keepShellEndpoint(ctx, &plan, o)
	return plan, mergeUserSettings(&plan)
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

func refuseProvider(sel providers.Selection) error {
	if p := sel.Provider; p.Auth.Login != "" {
		return fmt.Errorf("profile %s is the %s sign-in and cannot back Claude Code; use claude-code %s --profile %s", p.Name, p.Auth.Login, p.Auth.Login, p.Name)
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

// keepShellEndpoint lets a shell ANTHROPIC_MODEL (no --model flag) or ANTHROPIC_BASE_URL win over the vault resolution
func keepShellEndpoint(ctx context.Context, plan *agent.Plan, o agent.LaunchOptions) {
	keep := []string{proc.EnvAnthropicBaseURL}
	if o.Model == "" {
		keep = append(keep, "ANTHROPIC_MODEL")
	}
	own := plan.Set[proc.EnvAnthropicBaseURL]
	if own == "" {
		own = defaultAnthropicBase // an unset plan base means the default route
	}
	base := gateway.UntaggedBase(strings.TrimSpace(proc.Value(ctx, proc.EnvAnthropicBaseURL))) // a parent dispatch's /d/<id> tag
	inherited := inheritedBase(ctx, base, own)
	if base != "" && !inherited && !sameEndpoint(base, own) {
		plan.ForeignBase = true
		dropHarnessCredential(plan)
	}
	keepShellKeys(ctx, plan, keep, inherited)
}

func keepShellKeys(ctx context.Context, plan *agent.Plan, keys []string, inherited bool) {
	for _, k := range keys {
		if strings.TrimSpace(proc.Value(ctx, k)) == "" || (k == proc.EnvAnthropicBaseURL && inherited) {
			continue
		}
		delete(plan.Set, k)
		plan.Unset = slices.DeleteFunc(plan.Unset, func(u string) bool { return u == k })
		plan.Keep = append(slices.Clone(plan.Keep), k)
	}
}

// sessionProxyBase reports raw is the proxy base this harness session was launched on (/p/<CLAUDE_PROFILE>).
func sessionProxyBase(ctx context.Context, raw string) bool {
	u, err := url.Parse(raw)
	sp := providers.SessionProfileContext(ctx)
	if err != nil || sp == "" || strings.TrimRight(u.Path, "/") != "/p/"+sp {
		return false
	}
	st, err := readProxyState() // a /p/<profile> base on another port is the user's own override, not this session's
	return err == nil && st.Port != 0 && u.Port() == strconv.Itoa(st.Port)
}

// inheritedBase: the shell base is this session's (proxy, /p/<profile>, direct route); a hop drops it, keeps its login.
func inheritedBase(ctx context.Context, base, own string) bool {
	if isProxySessionBase(base) && (isProxySessionBase(own) && sameHostPort(base, own) || sessionProxyBase(ctx, base)) {
		return true
	}
	return sessionRouteBase(ctx, base)
}

// sessionRouteBase reports raw is the session profile's own Anthropic route: a direct, unproxied Databricks session.
func sessionRouteBase(ctx context.Context, raw string) bool {
	sp := providers.SessionProfileContext(ctx)
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

// launchDoc is the plan's --settings layer and its file path ("" when inline); nil when the plan has none.
func launchDoc(plan agent.Plan) (map[string]any, string, error) {
	if len(plan.Argv) < 2 || plan.Argv[0] != "--settings" {
		return nil, "", nil
	}
	v, raw := plan.Argv[1], []byte(plan.Argv[1])
	path := ""
	if len(plan.Files) > 0 && plan.Files[0].Path == v {
		path, raw = v, plan.Files[0].Data
	}
	doc := map[string]any{}
	return doc, path, json.Unmarshal(raw, &doc)
}

// putLaunchDoc stores doc as the overlay file at path, or inline in the --settings value when path is "".
func putLaunchDoc(plan *agent.Plan, path string, doc map[string]any) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings overlay: %w", err)
	}
	plan.Argv = slices.Clone(plan.Argv)
	if path == "" {
		plan.Files, plan.Argv[1] = nil, string(data)
		return nil
	}
	plan.Files = []agent.LaunchFile{{Path: path, Data: data}}
	return nil
}

// dropKeyHelper removes the apiKeyHelper and its TTL var; the rest of the --settings layer stays.
func dropKeyHelper(plan *agent.Plan) {
	delete(plan.Set, "CLAUDE_CODE_API_KEY_HELPER_TTL_MS")
	doc, path, err := launchDoc(*plan)
	if doc == nil {
		return
	}
	if err == nil {
		delete(doc, "apiKeyHelper")
		err = putLaunchDoc(plan, path, doc)
	}
	if err != nil { // never ship a helper we meant to drop: lose the whole layer instead
		plan.Argv, plan.Files = slices.Clone(plan.Argv[2:]), nil
	}
}

// collapsedPrefix starts the plan warning that names what Normalize dropped from the merged settings.
const collapsedPrefix = "settings collapsed: "

// mergeUserSettings folds each --settings the user typed into the launch's one layer (a second flag replaces it).
func mergeUserSettings(plan *agent.Plan) error {
	doc, _, err := launchDoc(*plan)
	if err != nil || doc == nil {
		return err
	}
	var rest []string
	found, argv := false, plan.Argv[2:]
	for i := 0; i < len(argv); i++ {
		v, ok := strings.CutPrefix(argv[i], "--settings=")
		switch {
		case argv[i] == "--":
			rest, i = append(rest, argv[i:]...), len(argv)
			continue
		case argv[i] == "--settings" && i+1 < len(argv):
			v, ok, i = argv[i+1], true, i+1
		}
		if !ok {
			rest = append(rest, argv[i])
			continue
		}
		user, err := readSettings(v)
		if err != nil {
			return fmt.Errorf("%s: %w", settingsLabel(v), err)
		}
		doc, found = mergeDoc(doc, user), true
	}
	if !found {
		return nil
	}
	doc, changed, err := normalizeDoc(doc)
	if err != nil {
		return err
	}
	if len(changed) > 0 {
		plan.Warnings = append(plan.Warnings, collapsedPrefix+strings.Join(changed, "; "))
	}
	plan.Argv = append([]string{"--settings", ""}, rest...)
	return putLaunchDoc(plan, "", doc) // inline: a per-launch doc must not rewrite the profile's shared file
}

// mergeDoc lays user over base; autoMode and permissions lists append, like Claude Code's own merge.
func mergeDoc(base, user map[string]any) map[string]any {
	out := maps.Clone(base)
	for k, v := range user {
		b, bok := out[k].(map[string]any)
		u, uok := v.(map[string]any)
		if (k != "autoMode" && k != "permissions") || !bok || !uok {
			out[k] = v
			continue
		}
		m := maps.Clone(b)
		for lk, lv := range u {
			m[lk] = appendNew(m[lk], lv)
		}
		out[k] = m
	}
	return out
}

// appendNew appends ul's entries missing from bl when both are lists, else returns ul.
func appendNew(bl, ul any) any {
	b, ok1 := bl.([]any)
	u, ok2 := ul.([]any)
	if !ok1 || !ok2 {
		return ul
	}
	out := slices.Clone(b)
	for _, x := range u {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
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

func set1MOff(plan *agent.Plan, sel providers.Selection, p *providers.Provider) {
	if p.Serves1M() {
		return
	}
	for _, t := range sel.Aliases {
		if models.Has1M(t.ClientModel()) {
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

func sessionProviders(sel providers.Selection) []*providers.Provider {
	out := []*providers.Provider{sel.Provider}
	for _, t := range sel.Aliases {
		out = append(out, t.Provider)
	}
	return out
}

// armedHop: a headless launch on the armed profile uses /p/<p> even when NeedsRouter would leave it direct.
func armedHop(ctx context.Context, p *providers.Provider, args []string) bool {
	arm := router.ArmProfileContext(ctx)
	return arm != "" && p.Name == arm && agent.IsHeadless(args)
}

// configureRouting wires the router URL/headers when proxied, else the provider's route, and returns the apiKeyHelper.
func configureRouting(ctx context.Context, sel providers.Selection, p *providers.Provider, passthrough, proxied bool, self string, plan *agent.Plan, extra ...string) (helper, settings string, err error) {
	for _, t := range sel.Aliases {
		if err := providers.ClaudeCodeError(t.Provider, t.Model); err != nil {
			return "", "", err
		}
	}
	if proxied {
		return configureClaudeProxy(ctx, sel, p, passthrough, self, plan, extra...)
	}
	helper = textutil.QuoteSH(helperBin(self)) + " resolve --secret --profile " + textutil.QuoteSH(p.Name)
	name := "overlay-" + p.Name
	if id := providers.IdentityFrom(ctx); id != "" {
		helper += " --as " + textutil.QuoteSH(id)
		if slices.Contains(overlayIdentities, id) { // one file per identity: a concurrent launch must not swap helpers
			name += "-as-" + id
		}
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
	return helper, filepath.Join(paths.StateDir(), name+".json"), nil
}

// overlayIdentities are the --as values safe to put in an overlay file name; any other value fails at resolve.
var overlayIdentities = []string{
	providers.IdentityAuto, providers.IdentityUser, providers.IdentitySP, providers.IdentityHeadless,
}

func configureClaudeProxy(ctx context.Context, sel providers.Selection, p *providers.Provider, passthrough bool, self string, plan *agent.Plan, extra ...string) (string, string, error) {
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
	tok, err := proxyToken(ctx, base, sel, extra...)
	if err != nil {
		return "", "", err
	}
	plan.Set[ProxyKeyEnv] = tok
	if passthrough { // Authorization holds the login, so the token rides the client header
		hdrs = append(hdrs, gateway.ClientHeader+": "+tok)
	}
	plan.Set[proc.EnvAnthropicCustomHeaders] = strings.Join(hdrs, "\n")
	return textutil.QuoteSH(helperBin(self)) + " resolve --secret --proxy", filepath.Join(paths.StateDir(), "overlay-"+p.Name+"-proxy.json"), nil
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

// ProxyKeyEnv carries a proxied launch's session token to `resolve --proxy`
const ProxyKeyEnv = "HARNESS_PROXY_API_KEY"

// proxyToken is this process's session token on routerBase for the session, alias and extra profiles.
func proxyToken(ctx context.Context, routerBase string, sel providers.Selection, extra ...string) (string, error) {
	aliases := slices.Clone(extra)
	for _, t := range sel.Aliases {
		aliases = append(aliases, t.Provider.Name)
	}
	scope, err := agent.TokenScope(sel.Provider.Name, aliases...)
	if err != nil {
		return "", fmt.Errorf("proxy: %w", err)
	}
	tok, err := agent.SessionToken(ctx, routerBase, scope...)
	if err != nil {
		return "", fmt.Errorf("proxy: session token for profiles %s: %w", strings.Join(scope, ","), err)
	}
	return tok, nil
}

func claudeLaunchBinaries(o agent.LaunchOptions) (string, string, error) {
	bin := o.Binary
	if bin == "" {
		var err error
		if bin, err = findClaude(); err != nil {
			return "", "", err
		}
	}
	self, err := selfBinary()
	return bin, self, err
}

// applyOverlay sets the passthrough OAuth handling or the apiKeyHelper, and writes the launch's one --settings layer.
func applyOverlay(ctx context.Context, plan *agent.Plan, passthrough bool, args []string, helper, path string, p *providers.Provider, rows []pickerRow) error {
	// Provider env (non-secret by Validate) rides the child's process env: Claude Code reads it before settings load.
	for k, v := range providerEnv(ctx, p.Env) {
		if _, set := plan.Set[k]; !set { // the harness-resolved key wins
			plan.Set[k] = v
		}
	}
	doc, err := settings.LaunchSettings(p.Preset == providers.PresetDatabricks)
	if err != nil {
		return err
	}
	if passthrough { // no apiKeyHelper: Claude Code disables subscription OAuth whenever one is set
		passthroughOAuth(ctx, plan, args)
	} else {
		plan.Set["CLAUDE_CODE_API_KEY_HELPER_TTL_MS"] = "300000"
		doc["apiKeyHelper"] = helper
	}
	if len(rows) > 0 { // own file: a headless launch rewriting the shared overlay would drop a live session's rows
		doc["modelPicker"] = modelPicker{Options: rows}
		path = strings.TrimSuffix(path, ".json") + "-picker.json"
	}
	plan.Argv = []string{"--settings", path}
	return putLaunchDoc(plan, path, doc)
}

// passthroughOAuth: Remote Control refuses the setup-token interactively; headless prefers the long-lived token.
func passthroughOAuth(ctx context.Context, plan *agent.Plan, args []string) {
	if !agent.IsHeadless(args) {
		plan.Unset = append(plan.Unset, oauthKey)
	} else if proc.Value(ctx, oauthKey) == "" {
		if tok := providers.HeadlessOAuthToken(); tok != "" {
			plan.Set[oauthKey] = tok
		}
	}
}

// modelPicker is Claude Code's modelPicker setting; rows append to the built-in /model lineup.
type modelPicker struct {
	Options []pickerRow `json:"options"`
}

// pickerRow sends Model verbatim; BehavesAs is the known id Claude Code treats a prefixed id as.
type pickerRow struct {
	Model       string `json:"model"`
	Label       string `json:"label"`
	Description string `json:"description"`
	BehavesAs   string `json:"behavesAs"`
	route       string // "<Provider> · <profile>", completed with the proxy address once routing is known
}

// proxyAddr is the router host:port a proxied session talks to, or "" when direct.
func proxyAddr(plan *agent.Plan, proxied bool) string {
	if !proxied {
		return ""
	}
	u, err := url.Parse(plan.Set[proc.EnvAnthropicBaseURL])
	if err != nil {
		return ""
	}
	return u.Host
}

func routeSuffix(addr string) string {
	if addr == "" {
		return " · direct"
	}
	return " · proxy " + addr
}

func routeOf(p *providers.Provider) string {
	name := cmp.Or(p.Provider, p.Preset, "anthropic")
	return strings.ToUpper(name[:1]) + name[1:] + " · " + p.Name
}

func clip80(s string) string {
	if r := []rune(s); len(r) > 80 {
		return string(r[:80])
	}
	return s
}

// setAliasDescriptions names provider, profile and route on each alias row; owner env and vault values win.
func setAliasDescriptions(ctx context.Context, plan *agent.Plan, sel providers.Selection, addr string) {
	custom := providers.CustomEnv()
	for _, alias := range models.AliasNames {
		prefix := "ANTHROPIC_DEFAULT_" + strings.ToUpper(alias)
		if plan.Set[prefix+"_MODEL"] == "" {
			continue
		}
		_, owned := custom[prefix+"_MODEL_DESCRIPTION"]
		if owned || proc.Value(ctx, prefix+"_MODEL_DESCRIPTION") != "" {
			continue
		}
		p := sel.Provider
		if t, ok := sel.Aliases[alias]; ok && t.Provider != nil {
			p = t.Provider
		}
		plan.Set[prefix+"_MODEL_DESCRIPTION"] = clip80(routeOf(p) + routeSuffix(addr))
	}
}

// pickerPeers are a Databricks session's extra /model profiles; a login session gets none, to keep Remote Control.
func pickerPeers(reg *providers.Registry, p *providers.Provider) []*providers.Provider {
	if p.Preset != providers.PresetDatabricks {
		return nil
	}
	var out []*providers.Provider
	if a := reg.ProfileBy("anthropic", providers.AuthCLI); a != nil {
		out = append(out, a)
	}
	for _, n := range reg.ProfilesOf("databricks") {
		if d := reg.Providers[n]; d.Name != p.Name && d.Preset == providers.PresetDatabricks && providers.CredentialPresent(d) {
			out = append(out, d)
		}
	}
	return out
}

// modelPickerRows are the /model rows for the peer profiles and their names for the token scope; none headless.
func modelPickerRows(reg *providers.Registry, p *providers.Provider, args []string) ([]pickerRow, []string) {
	if agent.IsHeadless(args) {
		return nil, nil
	}
	var rows []pickerRow
	var names []string
	for _, peer := range pickerPeers(reg, p) {
		if r := peerRows(reg, peer); len(r) > 0 {
			rows, names = append(rows, r...), append(names, peer.Name)
		}
	}
	return rows, names
}

func peerRows(reg *providers.Registry, peer *providers.Provider) []pickerRow {
	where, databricks := "Anthropic", peer.Preset == providers.PresetDatabricks
	if databricks {
		where = "Databricks " + peer.Name
	}
	var rows []pickerRow
	for _, alias := range models.AliasNames {
		if peer.Models[alias] == "" || databricks && alias == models.Fable { // Databricks serves no Fable
			continue
		}
		t := providers.ParseTarget(reg, peer.Models[alias], peer)
		base, _ := models.Canonical(t.Model)
		fam, major, minor, ok := models.ParseClaudeID(base)
		if !ok {
			continue
		}
		label := fmt.Sprintf("%s %d", strings.ToUpper(fam[:1])+fam[1:], major)
		if minor > 0 {
			label += "." + strconv.Itoa(minor)
		}
		rows = append(rows, pickerRow{Model: peer.Name + ":" + t.ClientModel(), Label: label + " (" + where + ")",
			route: routeOf(peer), BehavesAs: base})
	}
	return rows
}

// providerEnv is the provider's non-secret Env minus keys the process env or the vault custom env already set.
func providerEnv(ctx context.Context, env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	custom := providers.CustomEnv()
	out := map[string]string{}
	for k, v := range env {
		if _, ok := custom[k]; ok || proc.Value(ctx, k) != "" {
			continue
		}
		out[k] = v
	}
	return out
}

// ClaudeBinary finds native claude, never bin/claude (the launcher): PATH outside BinDir, else the newest install.
func ClaudeBinary() (string, error) {
	bin, err := paths.NativeClaude()
	if err == nil || os.Getenv(paths.AgentBinDirEnv) != "" {
		return bin, err // the override is hermetic: never fall back to the real install
	}
	if bin := newestInstalledClaude(); bin != "" {
		return bin, nil
	}
	return "", errors.New("claude: not found on PATH outside " + paths.BinDir() +
		" or in ~/.local/share/claude/versions (claude-code install --tools=claude)")
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
