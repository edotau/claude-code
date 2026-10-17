// Package configure is the `claude-code configure` dialogue: provider, keys, models, agents, router,
// and doctor sections over the existing provider/secrets/vendors/launch verbs, piped or interactive.
// It is also launch.Vendor's auto-configure callback, scoped to one vendor via Vendor.
package configure

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
var Sections = []string{"provider", "keys", "models", "agents", "router", "doctor"}

// Options controls one configure pass; In/Out let tests fully control the dialogue.
type Options struct {
	Provider    string    // --provider: pick this one non-interactively (provider section)
	Vendor      string    // --vendor: scope keys/agents to one vendor
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
	allOK := true
	for _, s := range want {
		d.header(s)
		var ok bool
		switch s {
		case "provider":
			ok = runProvider(d)
		case "keys":
			ok, _ = runKeys(d)
		case "models":
			ok = runModels(d)
		case "agents":
			ok = runAgents(d)
		case "router":
			ok = runRouter(d)
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

func runProvider(d *dialog) bool {
	reg := d.reg
	names := reg.Names()
	cur := providers.Pin(providers.PinProvider)
	if cur == "" {
		cur = reg.Default
	}
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
	// On a terminal the picker is the list; printing it twice is what made the old dialogue look doubled.
	// --check/--dry-run report only, so they never open a picker that would sit waiting for a key.
	if d.o.Interactive && !d.o.Check && !d.o.DryRun && isStdinTTY() {
		rows := make([][2]string, len(names))
		start := 0
		for i, n := range names {
			rows[i] = [2]string{n, cred(n) + "  " + reg.Providers[n].Kind}
			if n == cur {
				start = i
			}
		}
		if idx, err := tui.Pick("provider — pick the session provider (current: "+cur+")", rows, start); err == nil {
			if idx < 0 || names[idx] == cur {
				d.printf("provider   pinned %s (kept)\n", cur)
				return true
			}
			return applyProvider(d, names[idx])
		}
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
	loggedIn, plan := claudeLoginStatus()
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

// claudeLoginStatus asks `claude auth status` (JSON) for the interactive session; plan is " (max)"-style or "".
func claudeLoginStatus() (loggedIn bool, plan string) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return false, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "auth", "status").Output()
	if err != nil {
		return false, ""
	}
	var st struct {
		LoggedIn         bool   `json:"loggedIn"`
		SubscriptionType string `json:"subscriptionType"`
	}
	if json.Unmarshal(out, &st) != nil {
		return false, ""
	}
	if st.SubscriptionType != "" {
		plan = " (" + st.SubscriptionType + ")"
	}
	return st.LoggedIn, plan
}

// consoleURLs are where each API key is minted; the key option offers to open the page before the paste.
var consoleURLs = map[string]string{
	"ANTHROPIC_API_KEY":  "https://console.anthropic.com/settings/keys",
	"GEMINI_API_KEY":     "https://aistudio.google.com/apikey",
	"OPENAI_API_KEY":     "https://platform.openai.com/api-keys",
	"OPENROUTER_API_KEY": "https://openrouter.ai/settings/keys",
}

// authOption is one way to satisfy an absent credential: a vendor's browser/device login, or a pasted key.
type authOption struct {
	label string
	argv  []string // login CLI to run attached to the terminal
	mint  []string // CLI that prints a long-lived token; captured and stored under env
	env   string   // env var a pasted (or minted) key is stored under (env.d/secrets.env)
}

// authOptions lists every way kt can be satisfied, browser login first; the dialog shows them all.
func authOptions(kt keyTarget) []authOption {
	var opts []authOption
	if len(kt.loginArgv) > 0 {
		opts = append(opts, authOption{label: "Sign in via browser (" + strings.Join(kt.loginArgv, " ") + ")", argv: kt.loginArgv})
	}
	if len(kt.mintArgv) > 0 {
		opts = append(opts, authOption{label: "Mint a long-lived token for unattended runs (" + strings.Join(kt.mintArgv, " ") +
			" → secrets.env " + kt.envVar + ")", mint: kt.mintArgv, env: kt.envVar})
	}
	if kt.envVar != "" {
		label := "Paste an API key (" + kt.envVar + ")"
		if u := consoleURLs[kt.envVar]; u != "" {
			label += " — create one at " + u
		}
		opts = append(opts, authOption{label: label, env: kt.envVar})
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
				return keyTarget{label: name, present: true, source: target.Provider.Name + " key", provider: target.Provider}
			}
		}
		return keyTarget{label: name, present: false, fixCmd: vendors.ConfigureHint(name)}
	}
	switch name {
	case "codex":
		if vendors.CodexChatGPTLoginPresent() {
			return keyTarget{label: name, present: true, source: "ChatGPT login (codex login)"}
		}
		kt := fallback()
		if !kt.present {
			kt.loginArgv = []string{"codex", "login"}
		}
		return kt
	case "copilot":
		if vendors.CopilotLoginPresent() {
			return keyTarget{label: name, present: true, source: "GH_TOKEN / gh auth status"}
		}
		kt := fallback()
		if !kt.present {
			kt.loginArgv = []string{"copilot", "login"}
		}
		return kt
	case "gemini":
		if vendors.GeminiLoginPresent() {
			return keyTarget{label: name, present: true, source: "Google login (~/.gemini/settings.json)"}
		}
		if p, ok := reg.Providers["gemini"]; ok && providers.CredentialPresent(p) {
			return keyTarget{label: name, present: true, source: "GEMINI_API_KEY", envVar: "GEMINI_API_KEY", provider: p}
		}
		return keyTarget{label: name, present: false, envVar: "GEMINI_API_KEY", loginArgv: []string{"gemini"},
			fixCmd: "claude-code secrets set GEMINI_API_KEY, or `gemini` → /auth → Login with Google"}
	case "opencode":
		if names := vendors.OpencodeLogins(); len(names) > 0 {
			return keyTarget{label: name, present: true, source: "opencode auth login: " + strings.Join(names, ", ")}
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

// runKeys reports every target's state, offering to resolve an absent one on a TTY.
func runKeys(d *dialog) (allOK bool, changed bool) {
	return runKeysChanged(d)
}

func runKeysChanged(d *dialog) (allOK, changed bool) {
	targets, err := keyTargets(d.reg, d.o.Vendor)
	if err != nil {
		d.println(err)
		return false, false
	}
	allOK = true
	for _, kt := range targets {
		if kt.present {
			status, detail := probe(kt)
			if status == "rejected" {
				d.printf("%-10s present but REJECTED (%s): %s\n", kt.label, kt.source, detail)
				if d.o.Interactive && !d.o.Check && kt.envVar != "" && resolveKey(d, kt) {
					changed = true
					continue
				}
				allOK = false
				continue
			}
			if status != "" && status != "login" {
				d.printf("%-10s present (%s; %s)\n", kt.label, kt.source, status)
				continue
			}
			d.printf("%-10s present (%s)\n", kt.label, kt.source)
			if len(kt.mintArgv) > 0 && !kt.headless && d.o.Interactive && !d.o.Check &&
				confirm(d, "Mint a long-lived token so unattended runs (harvest, ask, cron) survive /login expiry?") {
				if runAuthOption(d, kt, authOption{mint: kt.mintArgv, env: kt.envVar}) {
					changed = true
				}
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
		for i, o := range opts {
			rows = append(rows, [2]string{fmt.Sprintf("%d)", i+1), o.label})
		}
		rows = append(rows, [2]string{"-", "Skip for now (" + kt.fixCmd + ")"})
		idx, err := tui.Pick(kt.label+" — no credential found; how do you want to authenticate?", rows, 0)
		if err == nil {
			if idx < 0 || idx >= len(opts) {
				return false
			}
			chosen = idx
		}
	}
	if chosen < 0 {
		for i, o := range opts {
			if confirm(d, o.label+"?") {
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

// runAuthOption performs one option: the login CLI on the real terminal, or console page + hidden paste + secrets.env.
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
		if tok := runMint(o.mint); tok != "" {
			return storeSecret(d, o.env, tok)
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
	return storeSecret(d, o.env, strings.TrimSpace(val))
}

// storeSecret writes one credential to env.d/secrets.env and reports where it went (never the value).
func storeSecret(d *dialog, env, val string) bool {
	if err := secrets.SetFile(env, val); err != nil {
		d.printf("%s: %v\n", env, err)
		return false
	}
	d.printf("wrote %s to %s\n", env, secrets.EnvFile())
	return true
}

// runMint runs the token-minting CLI on the real terminal with stdout tee'd, and returns the token it printed.
func runMint(argv []string) string {
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

// runLogin execs a vendor's own login CLI attached to the real terminal.
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

func isStdinTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// --- models ---

func runModels(d *dialog) bool {
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
		if _, _, cerr := vendors.Configure(context.Background(), name, target, ""); cerr != nil {
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
func secretLine(d *dialog, prompt string) (string, error) {
	if isStdinTTY() && d.o.In == os.Stdin {
		return tui.Secret(prompt)
	}
	fmt.Fprint(d.out, prompt)
	val, err := readSecret(d.br)
	fmt.Fprintln(d.out)
	return val, err
}
