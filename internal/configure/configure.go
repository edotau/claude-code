// Package configure is the `claude-code configure` dialogue: provider, keys, models, agents, proxy,
// and doctor sections over the existing provider/secrets/agents/launch verbs, piped or interactive.
// It is also agent.LaunchPlan's auto-configure callback, scoped to one agent via Agent.
package configure

import (
	"bufio"
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
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/preflight"
	"github.com/edotau/claude-code/internal/copilotsdk"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/tui"
)

// Sections runs in this order when no section is named.
var Sections = []string{"provider", "models", "agents", "proxy", "github", "remote", "browser", "claude", "codex", "routing", "databricks", "doctor"}

// Options controls one configure pass; In/Out let tests fully control the dialogue.
type Options struct {
	Provider    string    // --provider: pick this one non-interactively (provider section)
	Agent       string    // --agent: scope keys/agents to one agent CLI
	DryRun      bool      // print what would change; write nothing
	Check       bool      // report only; never writes; Run exits 1 if anything is unset
	All         bool      // --all (provider page): check every provider's sign-in and log in where needed
	Profile     string    // --all [<profile>]: check only this registry row
	JSON        bool      // --format json (provider --all): emit the row verdicts as JSON
	Drop        []string  // --drop: top-level settings.json keys to remove on every render
	Keep        []string  // --keep: keys to stop dropping
	Interactive bool      // stdin+stderr are both a terminal; the caller decides this
	Home        string    // agent-config home override (tests); "" -> os.UserHomeDir
	In          io.Reader // "" -> os.Stdin
	Out         io.Writer // "" -> os.Stdout
}

func (o Options) reader() io.Reader {
	if o.In == nil {
		return os.Stdin
	}
	return o.In
}

func (o Options) writer() io.Writer {
	if o.Out == nil {
		return os.Stdout
	}
	return o.Out
}

// dialog carries one Run's shared state: a single buffered reader so sequential prompts don't lose
// each other's buffered-ahead bytes, and the loaded registry.
type dialog struct {
	o     Options
	reg   *providers.Registry
	br    *bufio.Reader
	out   io.Writer
	color bool // out is a terminal and NO_COLOR is unset: headers and status tokens get painted
	// pending holds keys entered in the menu until Submit; nil outside the menu (writes go straight through).
	pending map[string]stagedKey
	// pendingDeletes holds secret names staged for vault deletion; executed alongside pending on leaving
	// the secrets page (nil outside a staged session, same lifecycle as pending).
	pendingDeletes map[string]bool
	// changed is set by any home-page action that wrote config; runMenu re-renders settings on exit.
	changed bool
	// copilotCatalog caches the copilot SDK model catalog for the life of this dialog (one dialog, one fetch).
	copilotCatalog       []copilotsdk.Model
	copilotCatalogErr    error
	copilotCatalogLoaded bool
}

// printf writes one status line with its verdict tokens painted (byte-identical text when color is off).
func (d *dialog) printf(format string, args ...any) {
	fmt.Fprint(d.out, ansi.Status(d.color, fmt.Sprintf(format, args...)))
}

func (d *dialog) println(args ...any) { d.printf("%s\n", fmt.Sprint(args...)) }

func (d *dialog) header(section string) {
	d.println(tui.Accent(d.color, "== "+section+" =="))
}

// warmServedOnChange warms the served cache when a pin changed or the switch flipped on since the run's snapshot.
func warmServedOnChange(out io.Writer, skip bool, pins map[string]router.PinSpec, wasOn bool) {
	if skip || !providers.ServedRoutingOn() || (wasOn && maps.Equal(pins, agent.AgentPins())) {
		return
	}
	if reg, err := providers.Load(); err == nil {
		preflight.Warm(out, reg, time.Now())
	}
}

// Run executes the named section (empty = every section in Sections order); returns the CLI exit code.
func Run(o Options, section string) int {
	out := o.writer()
	want := Sections
	if section != "" {
		if !slices.Contains(Sections, section) {
			fmt.Fprintf(out, "configure: unknown section %q (%s)\n", section, strings.Join(Sections, "|"))
			return 2
		}
		want = []string{section}
	}
	if len(o.Drop)+len(o.Keep) > 0 && o.Agent != "" && o.Agent != "claude" {
		fmt.Fprintln(out, "configure: --drop/--keep apply to claude settings only")
		return 2
	}
	if len(o.Drop)+len(o.Keep) > 0 && (section != "" || o.Provider != "") {
		fmt.Fprintln(out, "configure: --drop/--keep take no section or --provider")
		return 2
	}
	reg, err := providers.Load()
	if err != nil {
		fmt.Fprintf(out, "configure: %v\n", err)
		return 1
	}
	o.In = o.reader() // nil → os.Stdin, so the TTY pickers recognise the real terminal
	d := &dialog{o: o, reg: reg, br: bufio.NewReader(o.In), out: out, color: ansi.Enabled(out)}
	defer warmServedOnChange(out, o.Check || o.DryRun, agent.AgentPins(), providers.ServedRoutingOn())
	if len(o.Drop)+len(o.Keep) > 0 {
		code := runDropKeep(d)
		if d.changed && !o.Check && !o.DryRun && !rerenderConfigs(d) && code == 0 {
			code = 1 // the list is saved but settings.json still holds the key
		}
		return code
	}
	seedOnFirstRun(d, section)
	if section == "" && o.Interactive && !o.Check && !o.DryRun && isStdinTTY() {
		return runMenu(d)
	}
	allOK := true
	for _, s := range want {
		if !o.JSON {
			d.header(s)
		}
		var ok bool
		switch s {
		case "provider":
			ok = runProvider(d)
		case "models":
			ok = runModels(d)
		case "agents":
			ok = runAgents(d)
		case "proxy":
			ok = runProxy(d)
		case "github":
			ok = runGithub(d)
		case "remote":
			ok = runRemote(d)
		case "browser":
			ok = runBrowser(d)
		case "claude":
			ok = runClaudeSettings(d)
		case "codex":
			ok = runCodexSettings(d)
			if !ok {
				return 1
			}
		case "routing":
			ok = runRouting(d)
		case "databricks":
			ok = runDatabricks(d)
		case "doctor":
			ok = launch.Doctor(out)
		}
		if !ok {
			allOK = false
		}
	}
	if d.changed && !o.Check && !o.DryRun {
		rerenderConfigs(d)
	}
	if o.Check && !allOK {
		return 1
	}
	return 0
}

// Agent runs the keys then agents sections scoped to name; changed reports whether a login or key
// write actually happened (telling launch.Agent it is worth re-resolving). Call only after a human
// has already said yes on a TTY — Agent itself still gates every login/write on o.Interactive.
func Agent(name string, interactive bool, out io.Writer) (changed bool, err error) {
	reg, err := providers.Load()
	if err != nil {
		return false, err
	}
	d := &dialog{
		o:   Options{Agent: name, Interactive: interactive, Out: out, In: os.Stdin},
		reg: reg, br: bufio.NewReader(os.Stdin), out: out, color: ansi.Enabled(out),
	}
	d.header("keys")
	_, changed = runKeysChanged(d)
	d.header("agents")
	runAgents(d)
	return changed, nil
}

func readLine(br *bufio.Reader) string {
	line, _ := br.ReadString('\n')
	return strings.TrimRight(line, "\r\n")
}

// yes defaults to true (the prompts are always "[Y/n]"): blank or anything starting with 'y'/'Y'. End of input
// is a decline, never a default yes — a piped dialogue that runs dry must not start a login or a paste.
func yes(br *bufio.Reader) bool {
	raw, err := br.ReadString('\n')
	if err != nil && raw == "" {
		return false
	}
	line := strings.ToLower(strings.TrimSpace(raw))
	return line == "" || strings.HasPrefix(line, "y")
}

// --- provider ---

// currentProvider is the configured profile (vault pin), falling back to the registry default when unpinned.
func currentProvider(reg *providers.Registry) string {
	if cur := providers.Pin(providers.PinProfile); cur != "" {
		return cur
	}
	return reg.Default
}

func runProvider(d *dialog) bool {
	if d.o.All {
		return runProviderAll(d)
	}
	reg := d.reg
	names := reg.Names()
	cur := currentProvider(reg)
	cred := func(n string) string {
		if providers.CredentialPresent(reg.Providers[n]) {
			return "cred:ok"
		}
		return "cred:missing"
	}
	if d.o.Provider != "" {
		if _, err := reg.Get(d.o.Provider); err != nil {
			d.println(err)
			return false
		}
		return applyProvider(d, d.o.Provider)
	}
	// `configure provider` on a terminal opens the same claude sub-picker the top-level menu's claude
	// row does; --check/--dry-run report only, so they never open a picker that would sit waiting for a key.
	if d.o.Interactive && !d.o.Check && !d.o.DryRun && isStdinTTY() {
		return runAgentMenu(d, "claude", claudeMenu(d))
	}
	for i, n := range names {
		mark := " "
		if n == cur {
			mark = "*"
		}
		d.printf("%s %d) %-13s %s\n", mark, i+1, n, cred(n))
	}
	if !d.o.Interactive || d.o.Check || d.o.DryRun {
		d.printf("configured profile: %s; to change: claude-code switch <profile>\n", cur)
		if sp := providers.SessionProfile(); sp != "" {
			d.printf("this session: %s\n", sp)
		}
		return true
	}
	fmt.Fprint(d.out, "pick a profile by number or name (blank = keep current): ")
	line := strings.TrimSpace(readLine(d.br))
	if line == "" {
		return true
	}
	if idx, err := strconv.Atoi(line); err == nil && idx >= 1 && idx <= len(names) {
		line = names[idx-1]
	}
	if _, err := reg.Get(line); err != nil {
		d.println(err)
		return false
	}
	return applyProvider(d, line)
}

func applyProvider(d *dialog, name string) bool {
	if d.o.Check || d.o.DryRun {
		d.printf("would pin profile: %s\n", name)
		return true
	}
	if err := providers.SetPins(map[string]string{providers.PinProfile: name}); err != nil {
		d.println(err)
		return false
	}
	d.printf("profile pinned: %s\n", name)
	return true
}

// --- keys ---

// keyTarget is one row `configure keys` reports on: a provider-level credential or an agent's own login.
type keyTarget struct {
	label     string
	present   bool
	source    string              // present: which source/login answered
	loginArgv []string            // absent + a login CLI exists: offer/run this
	envVar    string              // absent + API-key based: prompt writes this var
	fixCmd    string              // piped: the exact command to run instead
	provider  *providers.Provider // present + keyed: probed live so a stored-but-rejected key is not "present"
	mintArgv  []string            // Max plan: the CLI that mints a long-lived headless token (claude setup-token)
	headless  bool                // Max plan: a setup-token is stored, so unattended runs survive /login expiry
}

// setupTokenRE matches the long-lived token `claude setup-token` prints once the browser flow completes.
var setupTokenRE = regexp.MustCompile(`sk-ant-oat\d+-[A-Za-z0-9_-]{20,}`)

// maxPlanTarget fills the claude row for the Claude Max plan (passthrough): the interactive /login session as
// reported by `claude auth status`, plus whether a long-lived setup-token is stored for unattended runs.
func maxPlanTarget(kt keyTarget) keyTarget {
	kt.loginArgv = []string{"claude", "auth", "login"}
	kt.mintArgv = []string{"claude", "setup-token"}
	kt.envVar = providers.OAuthTokenEnv
	kt.fixCmd = "claude auth login (interactive) · claude setup-token → claude-code vault set " + providers.OAuthTokenEnv + " (headless)"
	kt.headless = providers.HeadlessOAuthToken() != ""
	loggedIn, plan := providers.ClaudeLoginStatus()
	kt.present = loggedIn || kt.headless
	switch {
	case loggedIn && kt.headless:
		kt.source = "claude.ai login" + plan + "; headless token set"
	case loggedIn:
		kt.source = "claude.ai login" + plan + "; no headless token"
	case kt.headless:
		kt.source = "headless token (" + providers.OAuthTokenEnv + "); not logged in interactively"
	}
	return kt
}

// consoleURLs are where each API key is minted; the key option offers to open the page before the paste.
var consoleURLs = map[string]string{
	"ANTHROPIC_API_KEY": "https://console.anthropic.com/settings/keys",
	"OPENAI_API_KEY":    "https://platform.openai.com/api-keys",
}

// authOption is one way to satisfy an absent credential: an agent's browser/device login, or a pasted key.
type authOption struct {
	action string   // one or two words, the picker's first column (Paste API key)
	desc   string   // one short line, the second column
	argv   []string // login CLI to run attached to the terminal
	mint   []string // CLI that prints a long-lived token; captured and stored under env
	env    string   // name a pasted (or minted) key is stored under in the vault
}

// label is the piped one-line form of the option.
func (o authOption) label() string { return o.action + " — " + o.desc }

// authOptions lists every way kt can be satisfied, browser login first; the dialog shows them all.
func authOptions(kt keyTarget) []authOption {
	var opts []authOption
	if len(kt.loginArgv) > 0 {
		opts = append(opts, authOption{action: "Browser login", desc: strings.Join(kt.loginArgv, " "), argv: kt.loginArgv})
	}
	if len(kt.mintArgv) > 0 {
		opts = append(opts, authOption{action: "Mint token", desc: strings.Join(kt.mintArgv, " ") + " (unattended runs)",
			mint: kt.mintArgv, env: kt.envVar})
	}
	if kt.envVar != "" {
		desc := kt.envVar
		if u := consoleURLs[kt.envVar]; u != "" {
			desc = "create at " + strings.TrimPrefix(u, "https://")
		}
		opts = append(opts, authOption{action: "Paste API key", desc: desc, env: kt.envVar})
	}
	return opts
}

func keyTargets(reg *providers.Registry, agentFilter string) ([]keyTarget, error) {
	var out []keyTarget
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
		kt := keyTarget{
			label: "claude", present: providers.CredentialPresent(p), source: secrets.Resolve(p).Source,
			envVar: p.Auth.Env, fixCmd: fix, provider: p,
		}
		if p.Auth.Method() == providers.AuthCLI {
			kt = maxPlanTarget(kt)
		}
		out = append(out, kt)
	}
	for _, name := range agent.Names() {
		if agentFilter != "" && name != agentFilter {
			continue
		}
		out = append(out, agentKeyTarget(reg, name))
	}
	return out, nil
}

func agentKeyTarget(reg *providers.Registry, name string) keyTarget {
	sel, err := providers.Select(reg, "")
	s, _ := agent.LookupSchema(name)
	fallback := func() keyTarget {
		if err == nil {
			target, note, rerr := s.ResolveTarget(reg, sel, false)
			// The agent's own stored config/login is a working setup, not a missing key.
			if errors.Is(rerr, agent.ErrOwnLogin) {
				return keyTarget{label: name, present: true, source: afterUsing(note)}
			}
			// A passthrough provider carries another agent's login, never a key this agent can use.
			if rerr == nil && target.Provider.Auth.Method() != providers.AuthCLI && providers.CredentialPresent(target.Provider) {
				return keyTarget{label: name, present: true, source: target.Provider.Name + " key",
					envVar: target.Provider.Auth.Env, provider: target.Provider}
			}
		}
		hint := "run `claude-code configure agents`"
		if s, ok := agent.LookupSchema(name); ok {
			hint = s.CLI.ConfigureHint
		}
		return keyTarget{label: name, present: false, fixCmd: hint}
	}
	if present, source := s.LoginStatus(); present {
		kt := keyTarget{label: name, present: true, source: source}
		// copilot's label is the only trace of its source: a gh account or env token has no sign-in to re-run
		if name != "copilot" || strings.HasSuffix(source, "copilot login") {
			kt.loginArgv = s.CLI.LoginArgv // rotate: re-run the CLI's own sign-in
		}
		return kt
	}
	if name == "gemini" { // agy has no key route, so nothing to fall back to
		return keyTarget{label: name, present: false, loginArgv: s.CLI.LoginArgv, fixCmd: "run `agy` to sign in with Google"}
	}
	kt := fallback()
	if !kt.present {
		kt.loginArgv = s.CLI.LoginArgv
	}
	return kt
}

func runKeysChanged(d *dialog) (allOK, changed bool) {
	if d.o.Interactive && !d.o.Check && !d.o.DryRun && isStdinTTY() {
		return keysMenu(d) // a terminal gets the pick-one menu; piped runs keep the line-by-line pass below
	}
	targets, err := keyTargets(d.reg, d.o.Agent)
	if err != nil {
		d.println(err)
		return false, false
	}
	allOK = true
	for _, kt := range targets {
		var ok, targetChanged bool
		if kt.present {
			ok, targetChanged = runPresentKey(d, kt)
		} else {
			ok, targetChanged = runAbsentKey(d, kt)
		}
		allOK = allOK && ok
		changed = changed || targetChanged
	}
	return allOK, changed
}

func runPresentKey(d *dialog, kt keyTarget) (ok, changed bool) {
	status, detail, age := probeCached(kt)
	if status == "rejected" {
		return runRejectedKey(d, kt, detail)
	}
	if status != "" && status != "login" {
		d.printf("%-10s present (%s; %s%s)\n", kt.label, kt.source, status, ageSuffix(age))
	} else {
		d.printf("%-10s present (%s)\n", kt.label, kt.source)
	}
	if len(kt.mintArgv) > 0 && !kt.headless && d.o.Interactive && !d.o.Check &&
		confirm(d, "Mint a long-lived token so unattended runs (update, ask, cron) survive /login expiry?") {
		return true, runAuthOption(d, kt, authOption{mint: kt.mintArgv, env: kt.envVar})
	}
	return true, false
}

func runRejectedKey(d *dialog, kt keyTarget, detail string) (ok, changed bool) {
	d.printf("%-10s present but REJECTED (%s): %s\n", kt.label, kt.source, detail)
	if !d.o.Interactive || d.o.Check {
		return false, false
	}
	if runRotate(d, kt) || (kt.envVar != "" && resolveKey(d, kt)) {
		return true, true
	}
	return false, false
}

func runAbsentKey(d *dialog, kt keyTarget) (ok, changed bool) {
	if d.o.Interactive && !d.o.Check && resolveKey(d, kt) {
		return true, true
	}
	d.printf("%-10s ABSENT: %s\n", kt.label, kt.fixCmd)
	return false, false
}

// probe live-checks a present keyed credential (4s cap); "" when there is nothing to probe.
func probe(kt keyTarget) (status, detail string) {
	if kt.provider == nil {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	return providers.ProbeCredential(ctx, kt.provider)
}

// probeTTL is how long a verified/rejected probe result is trusted before re-probing; "unreachable"
// is never cached — a transient network blip shouldn't stick for 6 hours.
const probeTTL = 6 * time.Hour

// probeCached reuses fresh vault probe results; env overrides and uncached values always probe live.
func probeCached(kt keyTarget) (status, detail string, age time.Duration) {
	if kt.provider == nil || kt.provider.Auth.Env == "" {
		status, detail = probe(kt)
		return status, detail, 0
	}
	env := kt.provider.Auth.Env
	meta, src := probeSource(env)
	if src != "vault" {
		status, detail = probe(kt)
		return status, detail, 0
	}
	if cached, cachedAge, ok := secrets.CachedProbe(meta[env], probeTTL, time.Now()); ok {
		return cached, "", cachedAge
	}
	setAt := meta[env].SetAt
	status, detail = probe(kt)
	if status == "verified" || status == "rejected" || status == "unreachable" {
		_ = secrets.RecordProbe(env, status, time.Now(), setAt) // a failed cache write leaves the live result intact
	}
	return status, detail, 0
}

// probeSource resolves env's launch source; unreadable vault metadata yields nil, forcing a live probe.
func probeSource(env string) (meta map[string]secrets.Meta, src string) {
	meta, err := secrets.LoadMeta()
	if err != nil {
		_, src, _, _ = secrets.Effective(env) // meta unreadable: fall back to the live-lookup ladder
		return nil, src
	}
	src, _ = secrets.Locate(env, meta)
	return meta, src
}

// ageSuffix renders a cached result's age (" 2h ago"), or "" when age is zero (a live probe).
func ageSuffix(age time.Duration) string {
	switch {
	case age <= 0:
		return ""
	case age < time.Hour:
		return fmt.Sprintf(" %dm ago", int(age/time.Minute))
	default:
		return fmt.Sprintf(" %dh ago", int(age/time.Hour))
	}
}

// afterUsing extracts the fallback description from a ResolveTarget note ("…; using <this>").
func afterUsing(note string) string {
	if _, after, ok := strings.Cut(note, "using "); ok {
		return after
	}
	return "its own login"
}

// resolveKey presents every way to satisfy kt — browser/device login and pasted key side by side — as a picker
// on a terminal, or one [Y/n] per option when piped; false means skipped, empty, or failed.
func resolveKey(d *dialog, kt keyTarget) bool {
	opts := authOptions(kt)
	if len(opts) == 0 {
		return false
	}
	chosen := -1
	if isStdinTTY() && d.o.In == os.Stdin {
		rows := make([][2]string, 0, len(opts)+1)
		for _, o := range opts {
			rows = append(rows, [2]string{o.action, o.desc})
		}
		rows = append(rows, [2]string{"Skip", kt.fixCmd})
		title := kt.label + " — no credential found; how do you want to authenticate?"
		if kt.envVar != "" {
			title = kt.label + " — no " + kt.envVar + "; how do you want to set it?"
		}
		idx, err := tui.PickBack(title, rows, 0)
		if err == nil {
			if idx < 0 || idx >= len(opts) {
				return false
			}
			chosen = idx
		}
	}
	if chosen < 0 {
		for i, o := range opts {
			if confirm(d, o.label()+"?") {
				chosen = i
				break
			}
		}
		if chosen < 0 {
			return false
		}
	}
	return runAuthOption(d, kt, opts[chosen])
}

// runAuthOption performs one option: the login CLI on the real terminal, or console page + hidden paste + the store.
func runAuthOption(d *dialog, kt keyTarget, o authOption) bool {
	if len(o.argv) > 0 {
		if err := runLogin(o.argv); err != nil {
			d.printf("%s: %v\n", kt.label, err)
			return false
		}
		d.printf("%-10s logged in\n", kt.label)
		return true
	}
	if len(o.mint) > 0 {
		tok, err := runMint(o.mint)
		if err != nil {
			d.printf("%s: %v\n", kt.label, err)
			return false
		}
		if tok != "" {
			return storeValue(d, o.env, tok)
		}
		d.printf("%s: no token found in the output; paste it instead\n", o.env)
	}
	if u := consoleURLs[o.env]; u != "" && confirm(d, "Open "+u+" in your browser to create the key?") {
		openBrowser(u)
	}
	val, err := secretLine(d, "Paste a value for "+o.env+" (input hidden, Enter when done): ")
	if err != nil || strings.TrimSpace(val) == "" {
		d.printf("%s: no value entered\n", o.env)
		return false
	}
	_, v := ParseAssignment(val)
	return storeValue(d, o.env, v)
}

// runMint tees stdout from the token-minting CLI and returns its token only when the command succeeds.
func runMint(argv []string) (string, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runMintContext(ctx, argv)
}

// attachedCommand is argv under ctx; cancel sends SIGINT first, and Go kills it after WaitDelay.
func attachedCommand(ctx context.Context, argv []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 3 * time.Second
	return cmd
}

func runMintContext(ctx context.Context, argv []string) (string, error) {
	if err := ctxErr(ctx, "mint token"); err != nil {
		return "", err
	}
	if !actionConfirmed("Run `" + strings.Join(argv, " ") + "` to mint a new token") {
		return "", nil
	}
	defer tui.Preserve()()
	var buf bytes.Buffer
	cmd := attachedCommand(ctx, argv)
	cmd.Stdin, cmd.Stderr = os.Stdin, os.Stderr
	cmd.Stdout = io.MultiWriter(os.Stdout, &buf)
	err := cmd.Run()
	if cerr := ctxErr(ctx, "mint token"); cerr != nil {
		return "", cerr
	}
	if err != nil {
		return "", err
	}
	return setupTokenRE.FindString(buf.String()), nil
}

// ctxErr wraps ctx's error under verb when ctx is already done, else nil — the one cancellation check
// shared by every exec.CommandContext caller here and in home.go's registerClaudeMCPContext.
func ctxErr(ctx context.Context, verb string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", verb, err)
	}
	return nil
}

// openBrowser hands url to the desktop opener; best-effort, the paste prompt follows either way.
func openBrowser(url string) {
	bin := "xdg-open"
	if runtime.GOOS == "darwin" {
		bin = "open"
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := exec.CommandContext(ctx, bin, url).Run(); err != nil {
			fmt.Fprintf(os.Stderr, "configure: warning: browser opener failed: %v\n", err)
		}
	}()
}

// actionConfirmed asks [y/N] (Esc cancels) before a terminal run launches a mint CLI; a stray Enter must
// never start a run that rewrites an agent's own auth settings. Non-terminal runs proceed. Sign-in itself
// needs no second confirm — picking its menu row already was the confirmation.
var actionConfirmed = func(question string) bool {
	if !isStdinTTY() {
		return true
	}
	return tui.ConfirmKey(question+"?", false)
}

// saveConfirmed is the one save prompt every configure page ends with: [Y/n], Enter or blank = yes, Esc = no.
// Non-terminal runs save.
var saveConfirmed = func(question string) bool {
	if !isStdinTTY() {
		return true
	}
	return tui.ConfirmKey(question+"?", true)
}

// confirmLeave asks to save n staged change(s) of what; false (nothing to save, or n/Esc) means discard them.
func confirmLeave(n int, what string) bool {
	return n > 0 && saveConfirmed(fmt.Sprintf("Save %d %s", n, what))
}

// enterRow is every configure page's last row: Enter leaves the page, asking first when it has unsaved changes.
func enterRow(detail string) [2]string { return [2]string{"Enter", detail} }

// runLogin execs an agent's own login CLI attached to the real terminal: picking the row that leads here
// already was the confirmation, so it runs directly rather than asking [y/N] a second time.
func runLogin(argv []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runLoginContext(ctx, argv)
}

func cliArgv(argv []string) ([]string, error) {
	name, ok := paths.VendorName(argv[0])
	if !ok {
		return argv, nil
	}
	bin, err := paths.VendorBin(name)
	if err != nil {
		return nil, err
	}
	resolved := slices.Clone(argv)
	resolved[0] = bin
	return resolved, nil
}

func runLoginContext(ctx context.Context, argv []string) error {
	argv, err := cliArgv(argv)
	if err != nil {
		return err
	}
	defer tui.Preserve()()
	cmd := attachedCommand(ctx, argv)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	if cerr := ctxErr(ctx, "login"); cerr != nil {
		return cerr
	}
	return err
}

func isStdinTTY() bool { return ansi.IsTerminal(os.Stdin) }

// --- models ---

// runModels is the `configure models` / "Models" row entry: a pick-one menu on a terminal, the plain
// report otherwise (--check/--dry-run/piped never open a picker that would sit waiting for a choice).
func runModels(d *dialog) bool {
	if d.o.Interactive && !d.o.Check && !d.o.DryRun && isStdinTTY() {
		return modelsMenu(d)
	}
	return modelsReport(d)
}

func modelsReport(d *dialog) bool {
	sel, err := providers.Select(d.reg, "")
	if err != nil {
		d.println(err)
		return false
	}
	for _, s := range models.Slots {
		if t, ok := sel.Slots[s]; ok {
			d.printf("  %-7s %-40s -> %s\n", s, sel.ClientModel(s), t.Spec())
		}
	}
	d.println("live catalog: claude-code models --live")
	return true
}

// --- agents ---

func runAgents(d *dialog) bool {
	agentList := agent.DefaultAgents
	if d.o.Agent != "" {
		agentList = []string{d.o.Agent}
	}
	_, err := agent.RenderConfigs(agent.RenderOptions{
		Agents: agentList, DryRun: d.o.DryRun || d.o.Check, Check: d.o.Check, Home: d.o.Home, Out: d.out,
	})
	if err != nil {
		d.println(err)
		return false
	}
	sel, err := providers.Select(d.reg, "")
	if err != nil {
		d.println(err)
		return false
	}
	ok := true
	for _, name := range agentList {
		s, _ := agent.LookupSchema(name)
		target, note, rerr := s.ResolveTarget(d.reg, sel, false)
		if errors.Is(rerr, agent.ErrOwnLogin) {
			d.printf("%-10s runs on %s\n", name, afterUsing(note))
			continue
		}
		if rerr != nil {
			d.printf("%-10s FIX: %v; %s\n", name, rerr, s.CLI.ConfigureHint)
			ok = false
			continue
		}
		_, _, cerr := s.Configure(context.Background(), target, "")
		if errors.Is(cerr, agent.ErrOwnLogin) { // e.g. opencode on its own Copilot sign-in: working, nothing to inject
			d.printf("%-10s runs on its own %s login (%s)\n", name, target.Provider.Name, target.Model)
			continue
		}
		if cerr != nil {
			d.printf("%-10s FIX: %v; %s\n", name, cerr, s.CLI.ConfigureHint)
			ok = false
			continue
		}
		d.printf("%-10s routes to %s (%s)\n", name, target.Provider.Name, target.Model)
	}
	return ok
}

// --- proxy ---

func runProxy(d *dialog) bool {
	sel, err := providers.Select(d.reg, "")
	if err != nil {
		d.println(err)
		return false
	}
	if !sel.NeedsRouter(d.reg) {
		d.println("not needed: the session provider serves every model alias directly")
		return true
	}
	if d.o.Check || d.o.DryRun {
		d.println("needed: a cross-provider model alias or fallback ladder is configured; would start it")
		return true
	}
	if !d.o.Interactive {
		d.println("needed: run `claude-code gateway proxy --start`, or relaunch")
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	base, err := proxy.Ensure(ctx)
	if err != nil {
		d.println(err)
		return false
	}
	d.printf("proxy running: %s\n", base)
	return true
}

// confirm asks a [Y/n] question: tui.Confirm on the controlling terminal, the dialogue reader otherwise (tests).
func confirm(d *dialog, question string) bool {
	if isStdinTTY() && d.o.In == os.Stdin {
		return tui.Confirm(question, true)
	}
	fmt.Fprintf(d.out, "%s [Y/n] ", question)
	return yes(d.br)
}

// secretLine reads a hidden value: tui.Secret (echo off) on the terminal, the dialogue reader otherwise.
// Every paste shows as a masked preview (live on the terminal), so a wrong or doubled paste shows before it is saved.
func secretLine(d *dialog, prompt string) (string, error) {
	if isStdinTTY() && d.o.In == os.Stdin {
		return tui.Secret(prompt, pastePreview)
	}
	fmt.Fprint(d.out, prompt)
	val := readLine(d.br)
	fmt.Fprintln(d.out)
	if _, v := ParseAssignment(val); v != "" {
		d.printf("  pasted  %s\n", pastePreview(val))
	}
	return val, nil
}

// pastePreview masks a value for display; a NAME=value paste previews only its value.
func pastePreview(raw string) string {
	_, v := ParseAssignment(raw)
	return secrets.Preview(v)
}
