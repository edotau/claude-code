// Package configure is the `claude-code configure` dialogue: provider, keys, models, agents, router,
// and doctor sections over the existing provider/secrets/vendors/launch verbs, piped or interactive.
// It is also launch.Vendor's auto-configure callback, scoped to one vendor via Vendor.
package configure

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/ansi"
	"github.com/edotau/claude-code/internal/generate"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/tui"
	"github.com/edotau/claude-code/internal/vendors"
)

// Sections runs in this order when no section is named.
var Sections = []string{"provider", "models", "agents", "router", "github", "remote", "browser", "doctor"}

// Options controls one configure pass; In/Out let tests fully control the dialogue.
type Options struct {
	Provider    string    // --provider: pick this one non-interactively (provider section)
	Vendor      string    // --agent: scope keys/agents to one agent CLI
	DryRun      bool      // print what would change; write nothing
	Check       bool      // report only; never writes; Run exits 1 if anything is unset
	Interactive bool      // stdin+stderr are both a terminal; the caller decides this
	Home        string    // vendor-config home override (tests); "" -> os.UserHomeDir
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
	// changed is set by any home-page action that wrote config; runMenu re-renders settings on exit.
	changed bool
}

// printf writes one status line with its verdict tokens painted (byte-identical text when color is off).
func (d *dialog) printf(format string, args ...any) {
	fmt.Fprint(d.out, ansi.Status(d.color, fmt.Sprintf(format, args...)))
}

func (d *dialog) println(args ...any) { d.printf("%s\n", fmt.Sprint(args...)) }

func (d *dialog) header(section string) {
	d.println(ansi.Style(d.color, ansi.Bold+ansi.Cyan, "== "+section+" =="))
}

// Run executes the named section (empty = every section in Sections order); returns the CLI exit code.
func Run(o Options, section string) int {
	out := o.writer()
	want := Sections
	if section != "" {
		if !contains(Sections, section) {
			fmt.Fprintf(out, "configure: unknown section %q (%s)\n", section, strings.Join(Sections, "|"))
			return 2
		}
		want = []string{section}
	}
	reg, err := providers.Load()
	if err != nil {
		fmt.Fprintf(out, "configure: %v\n", err)
		return 1
	}
	o.In = o.reader() // nil → os.Stdin, so the TTY pickers recognise the real terminal
	d := &dialog{o: o, reg: reg, br: bufio.NewReader(o.In), out: out, color: ansi.Enabled(out)}
	if section == "" && o.Interactive && !o.Check && !o.DryRun && isStdinTTY() {
		return runMenu(d)
	}
	allOK := true
	for _, s := range want {
		d.header(s)
		var ok bool
		switch s {
		case "provider":
			ok = runProvider(d)
		case "models":
			ok = runModels(d)
		case "agents":
			ok = runAgents(d)
		case "router":
			ok = runRouter(d)
		case "github":
			ok = runGithub(d)
		case "remote":
			ok = runRemote(d)
		case "browser":
			ok = runBrowser(d)
		case "doctor":
			ok = launch.Doctor(out)
		}
		if !ok {
			allOK = false
		}
	}
	if o.Check && !allOK {
		return 1
	}
	return 0
}

// Vendor runs the keys then agents sections scoped to name; changed reports whether a login or key
// write actually happened (telling launch.Vendor it is worth re-resolving). Call only after a human
// has already said yes on a TTY — Vendor itself still gates every login/write on o.Interactive.
func Vendor(name string, interactive bool, out io.Writer) (changed bool, err error) {
	reg, err := providers.Load()
	if err != nil {
		return false, err
	}
	d := &dialog{
		o:   Options{Vendor: name, Interactive: interactive, Out: out, In: os.Stdin},
		reg: reg, br: bufio.NewReader(os.Stdin), out: out, color: ansi.Enabled(out),
	}
	d.header("keys")
	_, changed = runKeysChanged(d)
	d.header("agents")
	runAgents(d)
	return changed, nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
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

// currentProvider is the pinned session provider, falling back to the registry default when unpinned.
func currentProvider(reg *providers.Registry) string {
	if cur := providers.Pin(providers.PinProvider); cur != "" {
		return cur
	}
	return reg.Default
}

func runProvider(d *dialog) bool {
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
		d.printf("pinned: %s; to change: claude-code switch <provider>\n", cur)
		return true
	}
	fmt.Fprint(d.out, "pick a provider by number or name (blank = keep current): ")
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
		d.printf("would pin provider: %s\n", name)
		return true
	}
	if err := providers.SetPins(map[string]string{providers.PinProvider: name}); err != nil {
		d.println(err)
		return false
	}
	d.printf("provider pinned: %s\n", name)
	return true
}

// --- keys ---

// keyTarget is one row `configure keys` reports on: a provider-level credential or a vendor's own login.
type keyTarget struct {
	label     string
	present   bool
	source    string              // present: which plane/login answered
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
	kt.fixCmd = "claude auth login (interactive) · claude setup-token → claude-code secrets set " + providers.OAuthTokenEnv + " (headless)"
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

// authOption is one way to satisfy an absent credential: a vendor's browser/device login, or a pasted key.
type authOption struct {
	action string   // one or two words, the picker's first column (Paste API key)
	desc   string   // one short line, the second column
	argv   []string // login CLI to run attached to the terminal
	mint   []string // CLI that prints a long-lived token; captured and stored under env
	env    string   // name a pasted (or minted) key is stored under (OS keyring, else env.d/secrets.env)
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

func keyTargets(reg *providers.Registry, vendorFilter string) ([]keyTarget, error) {
	var out []keyTarget
	if vendorFilter == "" {
		sel, err := providers.Select(reg, "")
		if err != nil {
			return nil, err
		}
		p := sel.Provider
		fix := "resolve the credential for " + p.Name + " (see `claude-code secrets status`)"
		if p.Auth.Env != "" {
			fix = "claude-code secrets set " + p.Auth.Env
		}
		kt := keyTarget{
			label: "claude", present: providers.CredentialPresent(p), source: secrets.Resolve(p).Source,
			envVar: p.Auth.Env, fixCmd: fix, provider: p,
		}
		if p.Auth.Type == providers.AuthPassthrough {
			kt = maxPlanTarget(kt)
		}
		out = append(out, kt)
	}
	for _, name := range vendors.Names() {
		if vendorFilter != "" && name != vendorFilter {
			continue
		}
		out = append(out, vendorKeyTarget(reg, name))
	}
	return out, nil
}

func vendorKeyTarget(reg *providers.Registry, name string) keyTarget {
	sel, err := providers.Select(reg, "")
	fallback := func() keyTarget {
		if err == nil {
			target, note, rerr := vendors.ResolveTarget(reg, name, sel, false)
			// The vendor's own stored config/login is a working setup, not a missing key.
			if errors.Is(rerr, vendors.ErrOwnLogin) {
				return keyTarget{label: name, present: true, source: afterUsing(note)}
			}
			// A passthrough provider carries another vendor's login, never a key this vendor can use.
			if rerr == nil && target.Provider.Auth.Type != "passthrough" && providers.CredentialPresent(target.Provider) {
				return keyTarget{label: name, present: true, source: target.Provider.Name + " key",
					envVar: target.Provider.Auth.Env, provider: target.Provider}
			}
		}
		return keyTarget{label: name, present: false, fixCmd: vendors.ConfigureHint(name)}
	}
	switch name {
	case "codex":
		if vendors.CodexChatGPTLoginPresent() {
			return keyTarget{label: name, present: true, source: "ChatGPT login (codex login)",
				loginArgv: []string{"codex", "login"}} // rotate: re-run the same ChatGPT sign-in
		}
		kt := fallback()
		if !kt.present {
			kt.loginArgv = []string{"codex", "login"}
		}
		return kt
	case "copilot":
		if a, ok := vendors.ResolveCopilotAuth(); ok {
			kt := keyTarget{label: name, present: true, source: a.Label()}
			if a.Source == "copilot login" || a.Source == "gh" {
				kt.loginArgv = []string{"copilot", "login"} // rotate: re-run copilot's own sign-in
			}
			return kt
		}
		kt := fallback()
		if !kt.present {
			kt.loginArgv = []string{"copilot", "login"}
		}
		return kt
	case "gemini":
		if vendors.GeminiLoginPresent() {
			return keyTarget{label: name, present: true, source: "Google login (agy)",
				loginArgv: []string{"agy"}} // rotate: re-run `agy` and sign in again
		}
		return keyTarget{label: name, present: false, loginArgv: []string{"agy"}, fixCmd: "run `agy` to sign in with Google"}
	case "opencode":
		if names := vendors.OpencodeLogins(); len(names) > 0 {
			return keyTarget{label: name, present: true, source: "opencode auth login: " + strings.Join(names, ", "),
				loginArgv: []string{"opencode", "auth", "login"}} // rotate: re-run opencode's own login
		}
		kt := fallback()
		if !kt.present {
			kt.loginArgv = []string{"opencode", "auth", "login"}
		}
		return kt
	default:
		return fallback()
	}
}

func runKeysChanged(d *dialog) (allOK, changed bool) {
	if d.o.Interactive && !d.o.Check && !d.o.DryRun && isStdinTTY() {
		return keysMenu(d) // a terminal gets the pick-one menu; piped runs keep the line-by-line pass below
	}
	targets, err := keyTargets(d.reg, d.o.Vendor)
	if err != nil {
		d.println(err)
		return false, false
	}
	allOK = true
	for _, kt := range targets {
		if kt.present {
			status, detail, age := probeCached(kt)
			if status == "rejected" {
				d.printf("%-10s present but REJECTED (%s): %s\n", kt.label, kt.source, detail)
				if d.o.Interactive && !d.o.Check {
					if runRotate(d, kt) || (kt.envVar != "" && resolveKey(d, kt)) {
						changed = true
						continue
					}
				}
				allOK = false
				continue
			}
			if status != "" && status != "login" {
				d.printf("%-10s present (%s; %s%s)\n", kt.label, kt.source, status, ageSuffix(age))
			} else {
				d.printf("%-10s present (%s)\n", kt.label, kt.source)
			}
			if len(kt.mintArgv) > 0 && !kt.headless && d.o.Interactive && !d.o.Check &&
				confirm(d, "Mint a long-lived token so unattended runs (harvest, ask, cron) survive /login expiry?") {
				if runAuthOption(d, kt, authOption{mint: kt.mintArgv, env: kt.envVar}, false) {
					changed = true
				}
				continue
			}
			continue
		}
		if d.o.Interactive && !d.o.Check {
			if resolveKey(d, kt) {
				changed = true
				continue
			}
		}
		allOK = false
		d.printf("%-10s ABSENT: %s\n", kt.label, kt.fixCmd)
	}
	return allOK, changed
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

// probeCached is probe with a TTL cache keyed off the credential's own metadata: a stored value
// (keyring or file) with a fresh verified/rejected result skips the live probe entirely, returning
// its age for display. An env-sourced value, or one with no cached row, always probes live (age 0);
// a fresh probe result is recorded back to metadata so the next render can serve it from cache.
func probeCached(kt keyTarget) (status, detail string, age time.Duration) {
	if kt.provider == nil || kt.provider.Auth.Env == "" {
		status, detail = probe(kt)
		return status, detail, 0
	}
	env := kt.provider.Auth.Env
	meta, src := probeSource(env)
	if src != "keyring" && src != "file" {
		status, detail = probe(kt)
		return status, detail, 0
	}
	if cached, cachedAge, ok := secrets.CachedProbe(meta[env], probeTTL, time.Now()); ok {
		return cached, "", cachedAge
	}
	status, detail = probe(kt)
	if status == "verified" || status == "rejected" || status == "unreachable" {
		secrets.RecordProbe(env, status, time.Now())
	}
	return status, detail, 0
}

// probeSource resolves env's current plane the same way a launch would. meta is nil when the meta
// file is unreadable (a zero Meta then reads back from it, same as "no cached row").
func probeSource(env string) (meta map[string]secrets.Meta, src string) {
	meta, err := secrets.LoadMeta()
	if err != nil {
		_, src, _, _ = secrets.Effective(env) // meta unreadable: fall back to the live-lookup ladder
		return nil, src
	}
	file := secrets.ReadAll()
	src, _ = secrets.Locate(env, meta, file)
	if src == "file" && len(file[env]) != meta[env].Len {
		return nil, src // hand-edited outside the harness: its cached probe no longer describes this value
	}
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
	return runAuthOption(d, kt, opts[chosen], false)
}

// runAuthOption performs one option: the login CLI on the real terminal, or console page + hidden paste + the store.
func runAuthOption(d *dialog, kt keyTarget, o authOption, forceFile bool) bool {
	if len(o.argv) > 0 {
		if err := runLogin(o.argv); err != nil {
			d.printf("%s: %v\n", kt.label, err)
			return false
		}
		d.printf("%-10s logged in\n", kt.label)
		return true
	}
	if len(o.mint) > 0 {
		if tok := runMint(o.mint); tok != "" {
			return storeValue(d, o.env, tok, forceFile)
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
	return storeValue(d, o.env, v, forceFile)
}

// runMint runs the token-minting CLI on the real terminal with stdout tee'd, and returns the token it printed.
func runMint(argv []string) string {
	if !actionConfirmed("Run `" + strings.Join(argv, " ") + "` to mint a new token") {
		return ""
	}
	var buf bytes.Buffer
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stderr = os.Stdin, os.Stderr
	cmd.Stdout = io.MultiWriter(os.Stdout, &buf)
	_ = cmd.Run()
	return setupTokenRE.FindString(buf.String())
}

// openBrowser hands url to the desktop opener; best-effort, the paste prompt follows either way.
func openBrowser(url string) {
	bin := "xdg-open"
	if runtime.GOOS == "darwin" {
		bin = "open"
	}
	_ = exec.Command(bin, url).Start()
}

// actionConfirmed asks [y/N] (Esc cancels) before a terminal run launches a mint CLI; a stray Enter must
// never start a run that rewrites a vendor's own auth settings. Non-terminal runs proceed. Sign-in itself
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

// runLogin execs a vendor's own login CLI attached to the real terminal: picking the row that leads here
// already was the confirmation, so it runs directly rather than asking [y/N] a second time.
func runLogin(argv []string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// readSecret reads one line, toggling local echo off only when br wraps the real controlling terminal
// (a test's fake reader is never os.Stdin, so it is read back plainly).
func readSecret(br *bufio.Reader) (string, error) {
	if isStdinTTY() {
		echo := exec.Command("stty", "echo")
		echo.Stdin = os.Stdin
		off := exec.Command("stty", "-echo")
		off.Stdin = os.Stdin
		_ = off.Run()
		defer echo.Run()
	}
	line := readLine(br)
	return line, nil
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
	agentList := generate.DefaultAgents
	if d.o.Vendor != "" {
		agentList = []string{d.o.Vendor}
	}
	_, err := generate.Run(generate.Options{
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
		target, note, rerr := vendors.ResolveTarget(d.reg, name, sel, false)
		if errors.Is(rerr, vendors.ErrOwnLogin) {
			d.printf("%-10s runs on %s\n", name, afterUsing(note))
			continue
		}
		if rerr != nil {
			d.printf("%-10s FIX: %v; %s\n", name, rerr, vendors.ConfigureHint(name))
			ok = false
			continue
		}
		_, _, cerr := vendors.Configure(context.Background(), name, target, "")
		if errors.Is(cerr, vendors.ErrOwnLogin) { // e.g. opencode on its own Copilot sign-in: working, nothing to inject
			d.printf("%-10s runs on its own %s login (%s)\n", name, target.Provider.Name, target.Model)
			continue
		}
		if cerr != nil {
			d.printf("%-10s FIX: %v; %s\n", name, cerr, vendors.ConfigureHint(name))
			ok = false
			continue
		}
		d.printf("%-10s routes to %s (%s)\n", name, target.Provider.Name, target.Model)
	}
	return ok
}

// --- router ---

func runRouter(d *dialog) bool {
	sel, err := providers.Select(d.reg, "")
	if err != nil {
		d.println(err)
		return false
	}
	if !sel.NeedsRouter(d.reg) {
		d.println("not needed: the session provider serves every slot directly")
		return true
	}
	if d.o.Check || d.o.DryRun {
		d.println("needed: a cross-provider slot or fallback ladder is configured; would start it")
		return true
	}
	if !d.o.Interactive {
		d.println("needed: run `claude-code router start`, or relaunch with --router")
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	base, err := router.Ensure(ctx)
	if err != nil {
		d.println(err)
		return false
	}
	d.printf("router running: %s\n", base)
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
	val, err := readSecret(d.br)
	fmt.Fprintln(d.out)
	if _, v := ParseAssignment(val); err == nil && v != "" {
		d.printf("  pasted  %s\n", pastePreview(val))
	}
	return val, err
}

// pastePreview masks a value for display; a NAME=value paste previews only its value.
func pastePreview(raw string) string {
	_, v := ParseAssignment(raw)
	return secrets.Preview(v)
}
