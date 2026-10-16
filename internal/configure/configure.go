// Package configure is the `claude-code configure` dialogue: provider, keys, models, agents, router,
// and doctor sections over the existing provider/secrets/vendors/launch verbs, piped or interactive.
// It is also launch.Vendor's auto-configure callback, scoped to one vendor via Vendor.
package configure

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

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
	o   Options
	reg *providers.Registry
	br  *bufio.Reader
	out io.Writer
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
	d := &dialog{o: o, reg: reg, br: bufio.NewReader(o.reader()), out: out}
	allOK := true
	for _, s := range want {
		fmt.Fprintf(out, "== %s ==\n", s)
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
		reg: reg, br: bufio.NewReader(os.Stdin), out: out,
	}
	fmt.Fprintln(out, "== keys ==")
	_, changed = runKeysChanged(d)
	fmt.Fprintln(out, "== agents ==")
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

// yes defaults to true (the prompts are always "[Y/n]"): blank or anything starting with 'y'/'Y'.
func yes(br *bufio.Reader) bool {
	line := strings.ToLower(strings.TrimSpace(readLine(br)))
	return line == "" || strings.HasPrefix(line, "y")
}

// --- provider ---

func runProvider(d *dialog) bool {
	reg, out := d.reg, d.out
	names := reg.Names()
	cur := providers.Pin(providers.PinProvider)
	if cur == "" {
		cur = reg.Default
	}
	for i, n := range names {
		mark := " "
		if n == cur {
			mark = "*"
		}
		cred := "cred:missing"
		if providers.CredentialPresent(reg.Providers[n]) {
			cred = "cred:ok"
		}
		fmt.Fprintf(out, "%s %d) %-13s %s\n", mark, i+1, n, cred)
	}
	if d.o.Provider != "" {
		if _, err := reg.Get(d.o.Provider); err != nil {
			fmt.Fprintln(out, err)
			return false
		}
		return applyProvider(d, d.o.Provider)
	}
	if !d.o.Interactive {
		fmt.Fprintf(out, "pinned: %s; to change: claude-code switch <provider>\n", cur)
		return true
	}
	if isStdinTTY() {
		rows := make([][2]string, len(names))
		start := 0
		for i, n := range names {
			cred := "cred:missing"
			if providers.CredentialPresent(reg.Providers[n]) {
				cred = "cred:ok"
			}
			rows[i] = [2]string{n, cred + "  " + reg.Providers[n].Kind}
			if n == cur {
				start = i
			}
		}
		if idx, err := tui.Pick("provider — pick the session provider", rows, start); err == nil {
			if idx < 0 || names[idx] == cur {
				return true
			}
			return applyProvider(d, names[idx])
		}
	}
	fmt.Fprint(out, "pick a provider by number or name (blank = keep current): ")
	line := strings.TrimSpace(readLine(d.br))
	if line == "" {
		return true
	}
	if idx, err := strconv.Atoi(line); err == nil && idx >= 1 && idx <= len(names) {
		line = names[idx-1]
	}
	if _, err := reg.Get(line); err != nil {
		fmt.Fprintln(out, err)
		return false
	}
	return applyProvider(d, line)
}

func applyProvider(d *dialog, name string) bool {
	if d.o.Check || d.o.DryRun {
		fmt.Fprintf(d.out, "would pin provider: %s\n", name)
		return true
	}
	if err := providers.SetPins(map[string]string{providers.PinProvider: name}); err != nil {
		fmt.Fprintln(d.out, err)
		return false
	}
	fmt.Fprintf(d.out, "provider pinned: %s\n", name)
	return true
}

// --- keys ---

// keyTarget is one row `configure keys` reports on: a provider-level credential or a vendor's own login.
type keyTarget struct {
	label     string
	present   bool
	source    string   // present: which plane/login answered
	loginArgv []string // absent + a login CLI exists: offer/run this
	envVar    string   // absent + API-key based: prompt writes this var
	fixCmd    string   // piped: the exact command to run instead
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
		out = append(out, keyTarget{
			label: "claude", present: providers.CredentialPresent(p), source: secrets.Resolve(p).Source,
			envVar: p.Auth.Env, fixCmd: fix,
		})
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
			// A passthrough provider carries another vendor's login, never a key this vendor can use.
			if target, _, rerr := vendors.ResolveTarget(reg, name, sel, false); rerr == nil &&
				target.Provider.Auth.Type != "passthrough" && providers.CredentialPresent(target.Provider) {
				return keyTarget{label: name, present: true, source: target.Provider.Name + " key"}
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
			return keyTarget{label: name, present: true, source: "GEMINI_API_KEY"}
		}
		return keyTarget{label: name, present: false, envVar: "GEMINI_API_KEY", fixCmd: "claude-code secrets set GEMINI_API_KEY"}
	default: // opencode: no subscription sign-in; only a keyed provider can serve it
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
		fmt.Fprintln(d.out, err)
		return false, false
	}
	allOK = true
	for _, kt := range targets {
		if kt.present {
			fmt.Fprintf(d.out, "%-10s present (%s)\n", kt.label, kt.source)
			continue
		}
		if d.o.Interactive && !d.o.Check {
			if resolveKey(d, kt) {
				changed = true
				continue
			}
		}
		allOK = false
		fmt.Fprintf(d.out, "%-10s ABSENT: %s\n", kt.label, kt.fixCmd)
	}
	return allOK, changed
}

// resolveKey offers kt's login or prompts for its key; false means declined, empty, or it failed.
func resolveKey(d *dialog, kt keyTarget) bool {
	switch {
	case len(kt.loginArgv) > 0:
		if !confirm(d, "Log in to "+kt.label+" now?") {
			return false
		}
		if err := runLogin(kt.loginArgv); err != nil {
			fmt.Fprintf(d.out, "%s: %v\n", kt.label, err)
			return false
		}
		fmt.Fprintf(d.out, "%-10s logged in\n", kt.label)
		return true
	case kt.envVar != "":
		val, err := secretLine(d, "Paste a value for "+kt.envVar+" (input hidden, Enter when done): ")
		if err != nil || strings.TrimSpace(val) == "" {
			fmt.Fprintf(d.out, "%s: no value entered\n", kt.envVar)
			return false
		}
		if err := secrets.SetFile(kt.envVar, strings.TrimSpace(val)); err != nil {
			fmt.Fprintf(d.out, "%s: %v\n", kt.envVar, err)
			return false
		}
		fmt.Fprintf(d.out, "wrote %s to %s\n", kt.envVar, secrets.EnvFile())
		return true
	default:
		return false
	}
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
		fmt.Fprintln(d.out, err)
		return false
	}
	for _, s := range models.Slots {
		if t, ok := sel.Slots[s]; ok {
			fmt.Fprintf(d.out, "  %-7s %-40s -> %s\n", s, sel.ClientModel(s), t.Spec())
		}
	}
	fmt.Fprintln(d.out, "live catalog: claude-code models --live")
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
		fmt.Fprintln(d.out, err)
		return false
	}
	sel, err := providers.Select(d.reg, "")
	if err != nil {
		fmt.Fprintln(d.out, err)
		return false
	}
	ok := true
	for _, name := range agentList {
		target, note, rerr := vendors.ResolveTarget(d.reg, name, sel, false)
		if rerr != nil {
			fmt.Fprintf(d.out, "%-10s FIX: %v; %s\n", name, rerr, vendors.ConfigureHint(name))
			ok = false
			continue
		}
		if note != "" {
			fmt.Fprintln(d.out, note)
		}
		if _, _, cerr := vendors.Configure(context.Background(), name, target, ""); cerr != nil {
			fmt.Fprintf(d.out, "%-10s FIX: %v; %s\n", name, cerr, vendors.ConfigureHint(name))
			ok = false
			continue
		}
		fmt.Fprintf(d.out, "%-10s routes to %s (%s)\n", name, target.Provider.Name, target.Model)
	}
	return ok
}

// --- router ---

func runRouter(d *dialog) bool {
	sel, err := providers.Select(d.reg, "")
	if err != nil {
		fmt.Fprintln(d.out, err)
		return false
	}
	if !sel.NeedsRouter(d.reg) {
		fmt.Fprintln(d.out, "not needed: the session provider serves every slot directly")
		return true
	}
	if d.o.Check || d.o.DryRun {
		fmt.Fprintln(d.out, "needed: a cross-provider slot or fallback ladder is configured; would start it")
		return true
	}
	if !d.o.Interactive {
		fmt.Fprintln(d.out, "needed: run `claude-code router start`, or relaunch with --router")
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	base, err := router.Ensure(ctx)
	if err != nil {
		fmt.Fprintln(d.out, err)
		return false
	}
	fmt.Fprintf(d.out, "router running: %s\n", base)
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
