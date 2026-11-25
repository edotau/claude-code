package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/claude"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
)

// stdoutf writes formatted text to stdout in one write; a failure is returned, prefixed with prefix when set.
func stdoutf(prefix, format string, a ...any) error {
	if _, err := fmt.Fprintf(os.Stdout, format, a...); err != nil {
		if prefix == "" {
			return err
		}
		return fmt.Errorf("%s: %w", prefix, err)
	}
	return nil
}

// printLines prints each line of a rendered view, returning the first stdout write error.
func printLines(lines []string) error {
	for _, l := range lines {
		if err := stdoutf("", "%s\n", l); err != nil {
			return err
		}
	}
	return nil
}

var modelsPage = helptext.Page{
	Usage: "claude-code models [flags]",
	About: "Shows which model each alias (opus, sonnet, haiku, fable) resolves to on\n" +
		"the session profile. --pin and --unpin write claude's agent record; --live\n" +
		"lists the profile's catalog.",
	Commands: [][2]string{{"discover", "live-list models per profile; --apply saves them"},
		{"preflight", "each agent's pin check; --live runs the pong round"},
		{"learn", "fold the dispatch records into the routing fit table"}},
	Hint: "An alias pin on claude's agent record beats the profile's own model.\n" +
		"claude-code models discover -h lists its flags.",
}

// cmdModels is the models verb: alias resolution, pins, the live catalog, and the discover subcommand.
func cmdModels(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "discover":
			return modelsDiscover(args[1:])
		case "preflight":
			return cmdPreflight(args[1:])
		case "learn":
			return modelsLearn(args[1:])
		}
	}
	fs := helptext.NewFlagSet("models", modelsPage)
	provider := fs.String("profile", "", "profile to resolve (default: pin, then registry default)")
	unpin := fs.Bool("unpin", false, "clear every model pin")
	live := fs.Bool("live", false, "list the models the profile serves (GET /models)")
	dryRun := fs.Bool("dry-run", false, "with --pin/--unpin: print the agent record change, write nothing")
	format := helptext.FormatFlag(fs)
	var pins []string
	fs.Func("pin", "alias=model, or all=model (repeatable)", func(v string) error {
		pins = append(pins, v)
		return nil
	})
	args, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if code := checkModelsArgs(args, *dryRun, *unpin, pins, *format); code != 0 {
		return code
	}
	reg, err := loadProfile(*provider)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	if code, handled := configure.ApplyModelPins(reg, *provider, *unpin, pins, modelsPreview(*dryRun)); handled {
		return code
	}
	sel, err := providers.Select(reg, *provider)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	return renderModelSelection(reg, sel, *format, *live)
}

// loadProfile loads the registry; a non-empty profile must be one of its rows.
func loadProfile(profile string) (*providers.Registry, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, err
	}
	if profile != "" {
		if _, err := reg.Get(profile); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

// modelsPreview is the agent-record diff printer for --dry-run, nil for a real write.
func modelsPreview(dryRun bool) func(func(map[string]vault.AgentRecord)) int {
	if !dryRun {
		return nil
	}
	return func(mutate func(map[string]vault.AgentRecord)) int { return printRecordDiff(mutate, nil) }
}

// checkModelsArgs refuses a stray argument, a --dry-run with nothing to preview, or a bad --format (exit 2).
func checkModelsArgs(rest []string, dryRun, unpin bool, pins []string, format string) int {
	if len(rest) > 0 {
		return helptext.FailUsage("models: unexpected argument %q", rest[0])
	}
	if dryRun && !unpin && len(pins) == 0 {
		return helptext.FailUsage("models: --dry-run previews --pin or --unpin; add one or drop --dry-run")
	}
	if err := helptext.CheckFormat(format); err != nil {
		return helptext.BadFormat("models", err)
	}
	return 0
}

func renderModelSelection(reg *providers.Registry, sel providers.Selection, format string, live bool) int {
	if live {
		return listLive(sel.Provider, format)
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, map[string]any{"profile": sel.Provider.Name, "proxy": sel.NeedsRouter(reg), "models": sel.SpecsByAlias()}); err != nil {
			return helptext.Fail("models: %v", err)
		}
		return 0
	}
	if err := printLines(providers.SelectionLines(reg, sel)); err != nil {
		return helptext.Fail("models: %v", err)
	}
	return 0
}

// listLive prints the provider's live model catalog (github-copilot's own SDK, every other provider's /models GET).
func listLive(p *providers.Provider, format string) int {
	var own func(context.Context) ([]providers.ModelInfo, error)
	if p.Provider == "github" {
		own = copilot.ListModels
	}
	cat, err := providers.LoadCatalog(p, own)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	if cat.Note != "" {
		fmt.Fprintln(os.Stderr, "claude-code: models:", cat.Note)
	}
	if format == helptext.JSON {
		return helptext.WriteJSONOrFail("models", cat.Rows())
	}
	if err := printLines(cat.TableLines()); err != nil {
		return helptext.Fail("models: %v", err)
	}
	return 0
}

var switchPage = helptext.Page{
	Usage: "claude-code switch <profile> [flags]",
	About: "Sets the profile on claude and each other agent pinned to a fixed profile of the same provider; pool pins, " +
		"own-login, account and off pins, and other providers are kept. Relaunch to apply. " +
		"One launch only: claude-code --profile <p>.",
	Hint: "claude-code configure provider --list shows each profile's credential state.",
}

// cmdSwitch moves claude and eligible fixed same-provider pins; other routes are retained.
func cmdSwitch(args []string) int {
	dryRun, args := helptext.ExtractBool(args, "dry-run")
	if len(args) == 0 && !dryRun || len(args) > 0 && isHelpVerb(args[0]) {
		switchUsage()
		return 0
	}
	if len(args) != 1 { // --dry-run without a profile is a usage error, not success
		switchUsage()
		return 2
	}
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("%v", err)
	}
	profile, err := switchTarget(reg, args[0])
	if err != nil {
		return helptext.Fail("%v", err)
	}
	if dryRun {
		return previewSwitchProfile(reg, profile)
	}
	return applySwitchProfile(reg, profile)
}

func applySwitchProfile(reg *providers.Registry, profile string) int {
	if err := configure.SwitchProfile(reg, profile); err != nil {
		return helptext.Fail("%v", err)
	}
	if err := stdoutf("switch", "profile pinned: %s (relaunch to apply)\n", profile); err != nil {
		return helptext.Fail("%v", err)
	}
	preflight.Warm(context.Background(), os.Stdout, reg, time.Now())
	return 0
}

// switchTarget rejects the pool name with a hint, else an unknown profile.
func switchTarget(reg *providers.Registry, name string) (string, error) {
	if name == agent.PoolPin {
		if len(reg.ProfilesOf(providers.GroupDatabricks)) == 0 {
			return "", fmt.Errorf("%q is the pool, not a profile: configure a Databricks profile first", name)
		}
		return "", fmt.Errorf("%q is the pool, not a profile: name one (%s); pool-pinned agents already rotate",
			name, strings.Join(reg.ProfilesOf(providers.GroupDatabricks), ", "))
	}
	p, err := reg.Get(name)
	if err != nil {
		return "", err
	}
	return p.Name, nil
}

func previewSwitchProfile(reg *providers.Registry, profile string) int {
	recs, err := vault.ReadAgentRecords(agent.RosterNames())
	if err != nil {
		return helptext.Fail("switch preview: %v", err)
	}
	after := maps.Clone(recs)
	configure.SwitchRecords(reg, after, profile)
	if err := printLines(providers.DryRunLines(recs, after, keptLines(reg, recs, profile))); err != nil {
		return helptext.Fail("switch preview: %v", err)
	}
	return 0
}

// switchUsage prints switch's page; helptext.ExtractBool pulls its one flag out, so this FlagSet only renders it.
func switchUsage() {
	fs := helptext.NewFlagSet("switch", switchPage)
	fs.Bool("dry-run", false, "print the agent record change, write nothing")
	fs.Usage()
}

var resolvePage = helptext.Page{
	Usage: "claude-code resolve [flags]",
	About: "Resolves the session profile's credential and prints it redacted; --secret prints the value " +
		"(the apiKeyHelper Claude Code calls). --proxy selects a scoped proxy session token, never the " +
		"client secret: the launch's HARNESS_PROXY_API_KEY (unless --profile is outside its scope), else one minted " +
		"for the calling process and cached; it dies with that process, so avoid $(...) subshells.",
}

// cmdResolve is the apiKeyHelper: --proxy selects the armed session token, else a token scoped to the parent process.
func cmdResolve(args []string) int {
	fs := helptext.NewFlagSet("resolve", resolvePage)
	profile := fs.String("profile", "", "profile name")
	viaRouter := fs.Bool("proxy", false, "select a scoped proxy session token")
	as := fs.String("as", "", "Databricks identity the launch carries: auto|user|sp|headless")
	refresh := fs.Bool("refresh", false, "with --proxy: mint a new session token, drop the cached")
	secret := helptext.SecretFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if *refresh && !*viaRouter {
		return helptext.FailUsage("resolve --refresh needs --proxy: a direct credential has no session token to renew")
	}
	tok, err := resolveToken(*viaRouter, *profile, *as, *refresh)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	return helptext.PrintSecrets("resolve", *secret, helptext.Secret{Value: tok})
}

// resolveToken is the armed or scoped proxy token with proxy, else the profile's credential.
func resolveToken(proxy bool, profile, as string, refresh bool) (string, error) {
	if proxy {
		pid := os.Getppid()
		if err := guardProxyBase(profile); err != nil {
			return "", err
		}
		if err := checkProfile(profile); err != nil {
			return "", err
		}
		if tok := os.Getenv(claude.ProxyKeyEnv); tok != "" && launchScopeCovers(tok, profile) { // armed, confined to its profile
			if refresh {
				return "", errors.New("resolve --refresh: an armed launch's token cannot be renewed here")
			}
			return armedProxyToken(tok, profile, pid)
		}
		tok, err := scopedProxyToken(context.Background(), profile, refresh, pid) // Background reads the launcher-set overlay env
		if err != nil {
			return "", fmt.Errorf("resolve --proxy: %w", err) // never falls back to the router secret
		}
		return tok, nil
	}
	reg, err := providers.Load()
	if err != nil {
		return "", err
	}
	sel, err := providers.Select(reg, profile)
	if err != nil {
		return "", err
	}
	return providers.Credential(providers.WithIdentity(context.Background(), as), sel.Provider)
}

// guardProxyBase refuses a session token when the caller's base is set but not a loopback /p/<profile> proxy base.
func guardProxyBase(profile string) error {
	base := os.Getenv(proc.EnvAnthropicBaseURL)
	if base == "" {
		return nil
	}
	u, err := url.Parse(base)
	if err == nil && textutil.IsLoopbackHost(u.Hostname()) && routeProfile(base) != "" {
		return nil
	}
	host := "unparseable"
	if err == nil {
		host = u.Host
	}
	audit.Log(audit.HookBlocked, "resolve --proxy", "session token withheld: base host "+host)
	return fmt.Errorf("resolve --proxy: base %s is not the harness proxy; relaunch with `claude-code --profile %s`",
		host, cmp.Or(profile, "<profile>"))
}

// checkProfile refuses a --profile the registry does not know; "" is the session profile.
func checkProfile(profile string) error {
	if profile == "" {
		return nil
	}
	reg, err := providers.Load()
	if err != nil {
		return err
	}
	_, err = providers.Select(reg, profile)
	return err
}

// launchScopeCovers: the launch token serves profile ("" or in its readable scope).
func launchScopeCovers(tok, profile string) bool {
	if profile == "" {
		return true
	}
	c, ok := proxy.PeekSessionClaims(tok)
	return ok && slices.Contains(c.Profiles, profile)
}

// resolveProxyToken, refreshProxyToken and sessionInvalid are the mint, cache and check seams of resolve --proxy.
var (
	resolveProxyToken         = proxy.ResolveProxyToken
	resolveProxyTokenFailover = proxy.ResolveProxyTokenWithFailover
	refreshProxyToken         = proxy.RefreshProxyToken
	sessionInvalid            = proxy.SessionInvalid
	authenticClaims           = proxy.AuthenticClaims
)

// armedProxyToken is tok, or its exact-scope re-mint once refused, never beyond the launch's own overlay fence.
func armedProxyToken(tok, profile string, pid int) (string, error) {
	route := routeProfile(os.Getenv(proc.EnvAnthropicBaseURL)) // the profile the refused request was made on
	c, signed := authenticClaims(tok)
	if signed { // verified claims, valid or refused, stay inside the launcher-set fence
		if err := withinFence(&c, c.Profiles); err != nil {
			return "", err
		}
	}
	remint, ok := armedRemint(tok, cmp.Or(profile, route), pid) // an explicit --profile beats the route
	if ok {
		return remintArmed(c, signed, cmp.Or(profile, route), profile != "", remint)
	}
	if !signed { // an unsigned token is still held to the env's fence
		if err := withinFence(nil, []string{cmp.Or(profile, route)}); err != nil {
			return "", err
		}
	}
	return tok, nil
}

// remintArmed re-mints a refused armed token for pid inside its own scope, failover and fence.
func remintArmed(c proxy.SessionClaims, signed bool, route string, explicit bool, pid int) (string, error) {
	profiles, failover, ctx := []string{route}, "", context.Background()
	if signed { // keep the launch's own scope, failover and fence, never the registry's
		profiles, failover = c.Profiles, c.Failover
		if explicit {
			profiles = []string{route}
		}
		if c.Open || len(c.Fence) > 0 {
			ctx = proxy.WithFence(ctx, c.Fence, c.Open)
		}
	} else if route == "" { // unverifiable claims: only the route is trusted, never --profile
		return "", errors.New("resolve --proxy: an armed launch's token scope cannot be recovered; relaunch it")
	} else if err := withinFence(nil, profiles); err != nil {
		return "", err
	}
	return remintExact(ctx, profiles, failover, pid)
}

// withinFence refuses profiles outside c's signed fence, else the env's overlay fence when c is legacy or unsigned.
func withinFence(c *proxy.SessionClaims, profiles []string) error {
	if c != nil && c.Open {
		return nil
	}
	fence := []string(nil)
	if c != nil && len(c.Fence) > 0 {
		fence = c.Fence
	} else {
		f, open, err := proxy.ConfinementFence(context.Background())
		if err != nil {
			return fmt.Errorf("resolve --proxy: %w; relaunch it", err)
		}
		if open {
			return nil
		}
		fence = f
	}
	for _, p := range profiles {
		if !slices.Contains(fence, p) {
			return fmt.Errorf("resolve --proxy: %s is outside the token's confinement fence; relaunch it", p)
		}
	}
	return nil
}

// remintExact re-mints a refused armed token for pid with exactly profiles and failover, never a wider set.
func remintExact(ctx context.Context, profiles []string, failover string, pid int) (string, error) {
	tok, err := resolveProxyTokenFailover(ctx, profiles, pid, failover)
	if err != nil {
		return "", fmt.Errorf("resolve --proxy: %w", err)
	}
	return tok, nil
}

// routeProfile is <p> of a /p/<p> proxy base, "" for any other base.
func routeProfile(base string) string {
	u, err := url.Parse(proxy.UntaggedBase(base))
	if err != nil {
		return ""
	}
	_, rest, ok := strings.Cut(u.Path, "/p/")
	name, _, _ := strings.Cut(rest, "/")
	if !ok {
		return ""
	}
	return name
}

// armedRemint: does a refused armed tok need a re-mint, bound to its own live process (not a helper shell) or fallback.
func armedRemint(tok, profile string, fallback int) (int, bool) {
	if !proxy.SessionRefused(sessionInvalid(tok, profile)) {
		return 0, false
	}
	if c, ok := proxy.PeekSessionClaims(tok); ok && c.PID > 0 {
		if start, err := proc.StartTime(c.PID); err == nil && start == c.Start {
			return c.PID, true
		}
	}
	return fallback, true
}

// scopedProxyToken mints (or reuses) the session token the launcher would arm for the process pid.
func scopedProxyToken(ctx context.Context, profile string, refresh bool, pid int) (string, error) {
	reg, err := providers.Load()
	if err != nil {
		return "", err
	}
	sel, err := providers.Select(reg, profile)
	if err != nil {
		return "", err
	}
	var aliases []string
	for _, t := range sel.Aliases {
		aliases = append(aliases, t.Provider.Name)
	}
	scope, err := agent.TokenScope(ctx, sel.Provider.Name, aliases...)
	if err != nil {
		return "", err
	}
	if refresh {
		return refreshProxyToken(ctx, scope, pid)
	}
	return resolveProxyToken(ctx, scope, pid)
}

// keptLines lists "agent: off (kept)" or "agent: <pin> (kept)" for each agent a switch to profile leaves alone.
func keptLines(reg *providers.Registry, recs map[string]vault.AgentRecord, profile string) []string {
	_, off, kept := configure.SwitchTargets(reg, recs, profile)
	var lines []string
	for _, n := range off {
		lines = append(lines, n+": off (kept)")
	}
	for _, n := range kept {
		rec := recs[n]
		rec.Route = providers.CompleteRoute(reg, rec.EffectiveRoute())
		lines = append(lines, n+": "+configure.PinLabel(rec)+" (kept)")
	}
	return lines
}

// printRecordDiff previews model-pin changes; switch uses one snapshot for both its diff and kept notes.
func printRecordDiff(apply func(map[string]vault.AgentRecord), notes []string) int {
	before, err := vault.ReadAgentRecords(agent.RosterNames())
	if err != nil {
		return helptext.Fail("agent record preview: %v", err)
	}
	after := maps.Clone(before)
	apply(after)
	if err := printLines(providers.DryRunLines(before, after, notes)); err != nil {
		return helptext.Fail("agent record preview: %v", err)
	}
	return 0
}

var modelsDiscoverPage = helptext.Page{
	Usage: "claude-code models discover [flags]",
	About: "Live-discover models for one profile, or every registry profile (sorted).\nPreview by default; --apply appends sealed, profile-scoped selectable entries.\n--drop removes ids and stops --apply re-adding them; --undrop reverses it;\nboth preview until --apply.\nNever changes pins/routes or treats listing as inference proof.",
	Hint:  "Vendor-owned catalogs are not rewritten. /discover-models defaults to --apply.",
}

func modelsDiscover(args []string) int {
	fs := helptext.NewFlagSet("models discover", modelsDiscoverPage)
	profile := fs.String("profile", "", "discover only this registry profile")
	apply := fs.Bool("apply", false, "append discovered additions to saved runtime pickers")
	dry := fs.Bool("dry-run", false, "preview only (the default); conflicts with --apply")
	verify := fs.Bool("verify", false, "with --apply, also probe saved ids for deprecated endpoints")
	var drop, undrop []string
	listFlag := func(name, usage string, dst *[]string) {
		fs.Func(name, usage, func(v string) error {
			*dst = append(*dst, splitIDs(v)...)
			return nil
		})
	}
	listFlag("drop", "remove ids from saved menus, block re-adding (comma list)", &drop)
	listFlag("undrop", "let discovery add dropped ids again (comma list)", &undrop)
	format := helptext.FormatFlag(fs)
	rest, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(rest) > 0 {
		return helptext.FailUsage("models discover: unexpected argument %q", rest[0])
	}
	if *apply && *dry {
		return helptext.FailUsage("models discover: --apply and --dry-run conflict; drop one")
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("models discover", err)
	}
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("models discover: cannot load provider registry: %v", err)
	}
	names, code := discoverNames(reg, *profile)
	if code != 0 {
		return code
	}
	if len(drop)+len(undrop) > 0 {
		return modelsDrop(names, *profile == "", drop, undrop, *apply, *format, reg)
	}
	results, ok := catalog.DiscoverPickerProfiles(reg, names, copilot.DiscoverGithubModels, *apply, *verify)
	if !ok {
		code = 1
	}
	return printDiscoverResults(results, *apply, *format, code)
}

// splitIDs is the non-empty comma-separated ids of v.
func splitIDs(v string) []string {
	return slices.DeleteFunc(strings.Split(strings.ReplaceAll(v, " ", ""), ","), func(s string) bool { return s == "" })
}

// modelsDrop is `models discover --drop|--undrop`: a preview unless apply, and no discovery runs.
func modelsDrop(names []string, global bool, drop, undrop []string, apply bool, format string, reg *providers.Registry) int {
	if len(drop) > 0 && len(undrop) > 0 {
		return helptext.FailUsage("models discover: --drop and --undrop conflict; run one at a time")
	}
	ids, un := drop, false
	if len(undrop) > 0 {
		ids, un = undrop, true
	}
	results, ok := catalog.DropPickerProfiles(reg, names, ids, un, global, apply)
	code := 0
	if !ok {
		code = 1
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, struct {
			Apply   bool                 `json:"apply"`
			Results []catalog.DropResult `json:"results"`
		}{apply, results}); err != nil {
			return helptext.Fail("models discover: cannot write JSON: %v", err)
		}
		return code
	}
	if err := printDropText(results, ids, un, apply); err != nil {
		return helptext.Fail("models discover: cannot write text output: %v", err)
	}
	return code
}

func printDropText(results []catalog.DropResult, ids []string, undrop, apply bool) error {
	verb, mode := "drop", "preview, nothing written; pass --apply to write"
	if undrop {
		verb = "undrop"
	}
	if apply {
		mode = "written"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s (%s)\n", verb, strings.Join(ids, ","), mode)
	found := map[string]bool{}
	for _, r := range results {
		where := cmp.Or(r.Profile, "saved menu")
		for _, id := range r.Removed {
			found[strings.TrimPrefix(id, "databricks-")] = true
			fmt.Fprintf(&b, "  - %s: %s\n", where, id)
		}
		if r.Error != "" {
			fmt.Fprintf(&b, "  error %s: %s\n", where, r.Error)
		}
	}
	for _, id := range ids {
		if !undrop && !found[strings.TrimPrefix(id, "databricks-")] {
			fmt.Fprintf(&b, "  %s is in no saved menu\n", id)
		}
	}
	_, err := io.WriteString(os.Stdout, b.String())
	return err
}

// discoverNames is the sorted registry profile names, or just profile; an unknown one exits 2.
func discoverNames(reg *providers.Registry, profile string) ([]string, int) {
	if profile == "" {
		names := reg.Names()
		slices.Sort(names)
		return names, 0
	}
	if _, err := reg.Get(profile); err != nil {
		return nil, helptext.FailUsage("models discover: unknown profile %q", profile)
	}
	return []string{profile}, 0
}

func printDiscoverResults(results []catalog.DiscoverResult, apply bool, format string, code int) int {
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, struct {
			Apply   bool                     `json:"apply"`
			Results []catalog.DiscoverResult `json:"results"`
		}{apply, results}); err != nil {
			return helptext.Fail("models discover: cannot write JSON: %v", err)
		}
		return code
	}
	if err := printDiscoverText(results); err != nil {
		return helptext.Fail("models discover: cannot write text output: %v", err)
	}
	return code
}

// printDiscoverText renders the results into one buffer and writes it once, so a stdout write error is returned.
func printDiscoverText(results []catalog.DiscoverResult) error {
	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "%s/%s: %s source=%s discovered=%d additions=%d\n", r.Provider, r.Profile, r.Status, cmp.Or(r.Source, "none"), r.Discovered, len(r.Additions))
		for _, m := range r.Additions {
			fmt.Fprintf(&b, "  + %s\n", m.ID)
		}
		if len(r.Dropped) > 0 {
			fmt.Fprintf(&b, "  skipped as dropped: %s\n", strings.Join(r.Dropped, ", "))
		}
		if len(r.Deprecated) > 0 {
			fmt.Fprintf(&b, "  dropped as unserved: %s\n", strings.Join(r.Deprecated, ", "))
		}
		if r.ProbeNote != "" {
			fmt.Fprintf(&b, "  %s\n", r.ProbeNote)
		}
		if len(r.Destinations) > 0 {
			fmt.Fprintf(&b, "  destinations: %s\n", strings.Join(r.Destinations, ", "))
		}
		if r.Note != "" {
			fmt.Fprintf(&b, "  %s\n", r.Note)
		}
	}
	b.WriteString("Saved entries are append-only choices, not current availability or inference proof. Native vendor catalogs are unchanged.\n")
	_, err := io.WriteString(os.Stdout, b.String())
	return err
}

// providerList prints every provider's credential state, or one provider's detail.
func providerList(name, format string) int {
	if err := helptext.CheckFormat(format); err != nil {
		return helptext.BadFormat("configure provider", err)
	}
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("%v", err)
	}
	if format == helptext.JSON {
		return providerListJSON(reg, name)
	}
	if name == "" {
		return printProviderTable(reg)
	}
	p, err := reg.Get(name)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	source, note := providers.CredSource(p)
	if err := printLines(providers.ProfileDetail(p, source, note)); err != nil {
		return helptext.Fail("configure provider: %v", err)
	}
	return 0
}

// printProviderTable prints the provider and agent-CLI summary of the profiles whose credential resolves now.
func printProviderTable(reg *providers.Registry) int {
	var agents []providers.AgentServer
	for _, v := range agent.Names() {
		agents = append(agents, providers.AgentServer{Name: v, Serves: func(p *providers.Provider) bool { return agent.CanServe(v, p) }})
	}
	ready := func(p *providers.Provider) bool { src, _ := providers.CredSource(p); return src != "missing" }
	if err := printLines(providers.ListTable(reg, ready, agents)); err != nil {
		return helptext.Fail("configure provider: %v", err)
	}
	return 0
}

// providerEntry is one provider in the JSON listing: its shape and where its credential resolves from, never the value.
type providerEntry struct {
	Name        string            `json:"name"`
	Protocol    string            `json:"protocol"`
	Dialects    []string          `json:"dialects"`
	Auth        string            `json:"auth"`
	Login       string            `json:"login,omitempty"`
	Credential  bool              `json:"credential"`
	Models      map[string]string `json:"models,omitempty"`
	Group       string            `json:"group"` // the family `--list` files it under (anthropic, openai, ... local)
	Source      string            `json:"source"`
	Note        string            `json:"note"`
	Host        string            `json:"host,omitempty"` // non-secret: preset host, else BaseURL's scheme+host
	AccountID   string            `json:"accountId,omitempty"`
	WorkspaceID string            `json:"workspaceId,omitempty"`
	Options     providers.Options `json:"options,omitzero"`
}

// newProviderEntry builds p's listing entry; p.Name is its registry key.
func newProviderEntry(p *providers.Provider) providerEntry {
	e := providerEntry{
		Name: p.Name, Protocol: p.Protocol, Dialects: p.Dialects(), Auth: p.Auth.Method(), Login: p.Auth.Login,
		Credential: providers.CredentialPresent(p), Models: p.Models, Group: p.Provider,
		Host: providers.AuthHost(p), AccountID: p.Auth.AccountID, WorkspaceID: p.Auth.WorkspaceID, Options: p.Options,
	}
	e.Source, e.Note = providers.CredSource(p)
	return e
}

// providerListJSON emits the provider listing: all providers, or the one named, never a credential.
func providerListJSON(reg *providers.Registry, name string) int {
	if name != "" {
		p, err := reg.Get(name)
		if err != nil {
			return helptext.Fail("%v", err)
		}
		return helptext.WriteJSONOrFail("configure provider", newProviderEntry(p))
	}
	names := reg.Names()
	list := make([]providerEntry, 0, len(names))
	for _, n := range names {
		list = append(list, newProviderEntry(reg.Providers[n]))
	}
	var session string
	if sel, err := providers.Select(reg, ""); err == nil && sel.Provider != nil {
		session = sel.Provider.Name
	}
	return helptext.WriteJSONOrFail("configure provider", struct {
		Session  string          `json:"session"`
		Default  string          `json:"default"`
		Fallback []string        `json:"fallback,omitempty"`
		List     []providerEntry `json:"list"`
	}{session, reg.Default, reg.Fallback, list})
}

// modelPinAliases offers "alias=" stems for --pin; the user types the model id after the "=" themselves.
func modelPinAliases() []string {
	out := make([]string, 0, len(models.AliasNames)+1)
	for _, s := range models.AliasNames {
		out = append(out, s+"=")
	}
	return append(out, "all=")
}

// modelsSpec is the completion node for the models verb.
func modelsSpec() *helptext.Node {
	discover := &helptext.Node{Flags: []helptext.Flag{helptext.ValuedFlag("profile", providers.ProfileNames), helptext.BoolFlag("apply"),
		helptext.BoolFlag("dry-run"), {Name: "drop"}, {Name: "undrop"},
		helptext.BoolFlag("verify"), helptext.CompletionFormatFlag()}}
	return &helptext.Node{Sub: map[string]*helptext.Node{"discover": discover, "preflight": preflightSpec()}, Flags: []helptext.Flag{helptext.ValuedFlag("profile", providers.ProfileNames),
		helptext.BoolFlag("unpin"), helptext.BoolFlag("live"), {Name: "pin", Values: modelPinAliases}, helptext.BoolFlag("dry-run"), helptext.CompletionFormatFlag()}}
}

// switchSpec is the completion node for the switch verb.
func switchSpec() *helptext.Node {
	return &helptext.Node{Args: []func() []string{providers.ProfileNames}, Flags: []helptext.Flag{helptext.BoolFlag("dry-run")}}
}

// resolveSpec is the completion node for the resolve verb.
func resolveSpec() *helptext.Node {
	identities := func() []string { return []string{"auto", "user", "sp", "headless"} }
	return &helptext.Node{Flags: []helptext.Flag{helptext.ValuedFlag("profile", providers.ProfileNames), helptext.BoolFlag("proxy"),
		helptext.ValuedFlag("as", identities), helptext.BoolFlag("refresh"), helptext.BoolFlag("secret")}}
}
