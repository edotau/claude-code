package menu

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"github.com/edotau/claude-code/internal/agent"
	agentrender "github.com/edotau/claude-code/internal/agentconfig/render"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secretstore"
	"github.com/edotau/claude-code/internal/ui/ansi"
	uidialog "github.com/edotau/claude-code/internal/ui/dialog"
	"github.com/edotau/claude-code/internal/ui/tui"
	"io"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// SectionNames runs in this order when no section is named.
var SectionNames = []string{"provider", "models", "agents", "github", "remote", "claude", "codex", "routing", "databricks"}

// Options controls one configure pass; In/Out let tests fully control the dialogue.
type Options struct {
	Provider    string    // --profile: pick this one non-interactively (provider section)
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
	copilotCatalog       []copilot.SDKModel
	copilotCatalogErr    error
	copilotCatalogLoaded bool
}

// printf writes one status line with its verdict tokens painted (byte-identical text when color is off).
func (d *dialog) printf(format string, args ...any) {
	fmt.Fprint(d.out, ansi.Status(d.color, fmt.Sprintf(format, args...)))
}

// shell is the ui/dialog shell on d's terminal or numbered prompt, painting status lines like printf.
func (d *dialog) shell() *uidialog.Shell {
	return &uidialog.Shell{Out: d.out, In: d.br, TTY: isStdinTTY() && d.o.In == os.Stdin,
		Paint: func(s string) string { return ansi.Status(d.color, s) }}
}

func (d *dialog) println(args ...any) { d.printf("%s\n", fmt.Sprint(args...)) }

func (d *dialog) header(section string) {
	d.println(tui.Accent(d.color, Heading(section)))
}

// Run executes the named section (empty = every section in SectionNames order); returns the CLI exit code.
func Run(o Options, section string) int {
	out := o.writer()
	want := SectionNames
	if section != "" {
		if !slices.Contains(SectionNames, section) {
			fmt.Fprintf(out, "configure: unknown section %q (%s)\n", section, strings.Join(SectionNames, "|"))
			return 2
		}
		want = []string{section}
	}
	if len(o.Drop)+len(o.Keep) > 0 && o.Agent != "" && o.Agent != "claude" {
		fmt.Fprintln(out, "configure: --drop/--keep apply to claude settings only")
		return 2
	}
	if len(o.Drop)+len(o.Keep) > 0 && (section != "" || o.Provider != "") {
		fmt.Fprintln(out, "configure: --drop/--keep take no section or --profile")
		return 2
	}
	reg, err := providers.Load()
	if err != nil {
		fmt.Fprintf(out, "configure: %v\n", err)
		return 1
	}
	o.In = o.reader() // nil → os.Stdin, so the TTY pickers recognise the real terminal
	d := &dialog{o: o, reg: reg, br: bufio.NewReader(o.In), out: out, color: ansi.Enabled(out)}
	defer configure.WarmServedOnChange(out, o.Check || o.DryRun, agent.AgentPins(), providers.ServedRoutingOn())
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
		case "github":
			ok = runGithub(d)
		case "remote":
			ok = runRemote(d)
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

func runProvider(d *dialog) bool {
	if d.o.All {
		return runProviderAll(d)
	}
	reg := d.reg
	names := reg.Names()
	cur := configure.CurrentProvider(reg)
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
	line := strings.TrimSpace(uidialog.ReadLine(d.br))
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
	if _, err := writeAgentField(d, "claude", "PROFILE", name); err != nil {
		d.println(err)
		return false
	}
	d.printf("profile pinned: %s\n", name)
	if d.o.Interactive && isStdinTTY() {
		offerModels(d, name)
	}
	return true
}

func runKeysChanged(d *dialog) (allOK, changed bool) {
	if d.o.Interactive && !d.o.Check && !d.o.DryRun && isStdinTTY() {
		return keysMenu(d) // a terminal gets the pick-one menu; piped runs keep the line-by-line pass below
	}
	targets, err := configure.KeyTargets(d.reg, d.o.Agent)
	if err != nil {
		d.println(err)
		return false, false
	}
	allOK = true
	for _, kt := range targets {
		var ok, targetChanged bool
		if kt.Present {
			ok, targetChanged = runPresentKey(d, kt)
		} else {
			ok, targetChanged = runAbsentKey(d, kt)
		}
		allOK = allOK && ok
		changed = changed || targetChanged
	}
	return allOK, changed
}

func runPresentKey(d *dialog, kt configure.KeyTarget) (ok, changed bool) {
	status, detail, age := configure.ProbeCached(kt)
	if status == "rejected" {
		return runRejectedKey(d, kt, detail)
	}
	if status != "" && status != "login" {
		d.printf("%-10s present (%s; %s%s)\n", kt.Label, kt.Source, status, ageSuffix(age))
	} else {
		d.printf("%-10s present (%s)\n", kt.Label, kt.Source)
	}
	if len(kt.MintArgv) > 0 && !kt.Headless && d.o.Interactive && !d.o.Check &&
		d.shell().Confirm("Mint a long-lived token so unattended runs (update, ask, cron) survive /login expiry?") {
		return true, runAuthOption(d, kt, configure.AuthOption{Mint: kt.MintArgv, Env: kt.EnvVar})
	}
	return true, false
}

func runRejectedKey(d *dialog, kt configure.KeyTarget, detail string) (ok, changed bool) {
	d.printf("%-10s present but REJECTED (%s): %s\n", kt.Label, kt.Source, detail)
	if !d.o.Interactive || d.o.Check {
		return false, false
	}
	if runRotate(d, kt) || (kt.EnvVar != "" && resolveKey(d, kt)) {
		return true, true
	}
	return false, false
}

func runAbsentKey(d *dialog, kt configure.KeyTarget) (ok, changed bool) {
	if d.o.Interactive && !d.o.Check && resolveKey(d, kt) {
		return true, true
	}
	d.printf("%-10s ABSENT: %s\n", kt.Label, kt.FixCmd)
	return false, false
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

// resolveKey presents every way to satisfy kt — browser/device login and pasted key side by side — as a picker
// on a terminal, or one [Y/n] per option when piped; false means skipped, empty, or failed.
func resolveKey(d *dialog, kt configure.KeyTarget) bool {
	opts := configure.AuthOptions(kt)
	if len(opts) == 0 {
		return false
	}
	chosen := -1
	if isStdinTTY() && d.o.In == os.Stdin {
		rows := make([][2]string, 0, len(opts)+1)
		for _, o := range opts {
			rows = append(rows, [2]string{o.Action, o.Desc})
		}
		rows = append(rows, [2]string{"Skip", kt.FixCmd})
		title := kt.Label + " — no credential found; how do you want to authenticate?"
		if kt.EnvVar != "" {
			title = kt.Label + " — no " + kt.EnvVar + "; how do you want to set it?"
		}
		idx, err := d.shell().Pick(title, rows)
		if err == nil {
			if idx < 0 || idx >= len(opts) {
				return false
			}
			chosen = idx
		}
	}
	if chosen < 0 {
		for i, o := range opts {
			if d.shell().Confirm(o.Label() + "?") {
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
func runAuthOption(d *dialog, kt configure.KeyTarget, o configure.AuthOption) bool {
	if len(o.Argv) > 0 {
		if err := configure.RunLogin(o.Argv); err != nil {
			d.printf("%s: %v\n", kt.Label, err)
			return false
		}
		d.printf("%-10s logged in\n", kt.Label)
		return true
	}
	if len(o.Mint) > 0 {
		tok, err := runMint(o.Mint)
		if err != nil {
			d.printf("%s: %v\n", kt.Label, err)
			return false
		}
		if tok != "" {
			return storeValue(d, o.Env, tok)
		}
		d.printf("%s: no token found in the output; paste it instead\n", o.Env)
	}
	if u := configure.ConsoleURLs[o.Env]; u != "" && d.shell().Confirm("Open "+u+" in your browser to create the key?") {
		openBrowser(u)
	}
	val, err := secretLine(d, "Paste a value for "+o.Env+" (input hidden, Enter when done): ")
	if err != nil || strings.TrimSpace(val) == "" {
		d.printf("%s: no value entered\n", o.Env)
		return false
	}
	_, v := configure.ParseAssignment(val)
	return storeValue(d, o.Env, v)
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
var actionConfirmed = func(question string) bool { return uidialog.ConfirmTerminal(question, false) }

// saveConfirmed is the one save prompt every configure page ends with: [Y/n], Enter or blank = yes, Esc = no.
// Non-terminal runs save.
var saveConfirmed = func(question string) bool { return uidialog.ConfirmTerminal(question, true) }

// confirmLeave asks to save n staged change(s) of what; false (nothing to save, or n/Esc) means discard them.
func confirmLeave(n int, what string) bool {
	return n > 0 && saveConfirmed(fmt.Sprintf("Save %d %s", n, what))
}

func isStdinTTY() bool { return ansi.IsTerminal(os.Stdin) }

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
	for _, s := range models.AliasNames {
		if t, ok := sel.Aliases[s]; ok {
			d.printf("  %-7s %-40s -> %s\n", s, sel.ClientModel(s), t.Spec())
		}
	}
	d.println("live catalog: claude-code models --live")
	return true
}

func runAgents(d *dialog) bool {
	agentList := agent.DefaultAgents
	if d.o.Agent != "" {
		agentList = []string{d.o.Agent}
	}
	_, err := agent.RenderConfigs(agentrender.RenderOptions{
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
			d.printf("%-10s runs on %s\n", name, configure.AfterUsing(note))
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

// secretLine reads a hidden value: tui.Secret (echo off) on the terminal, the dialogue reader otherwise.
// Every paste shows as a masked preview (live on the terminal), so a wrong or doubled paste shows before it is saved.
func secretLine(d *dialog, prompt string) (string, error) {
	if isStdinTTY() && d.o.In == os.Stdin {
		return tui.Secret(prompt, pastePreview)
	}
	fmt.Fprint(d.out, prompt)
	val := uidialog.ReadLine(d.br)
	fmt.Fprintln(d.out)
	if _, v := configure.ParseAssignment(val); v != "" {
		d.printf("  pasted  %s\n", pastePreview(val))
	}
	return val, nil
}

// pastePreview masks a value for display; a NAME=value paste previews only its value.
func pastePreview(raw string) string {
	_, v := configure.ParseAssignment(raw)
	return secretstore.Preview(v)
}
