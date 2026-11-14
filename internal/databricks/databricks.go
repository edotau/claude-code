// Package databricks launches Claude Code on Databricks; also check, login, token, import, export, config, rescue.
package databricks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

const (
	smokePrompt = "Reply with exactly: OK"
	betaKey     = providers.BetaOffKey
	betaOff     = betaKey + "=1"
)

// launchPlan is one parsed invocation: the profile and model the child launches with, plus pass-through args.
type launchPlan struct {
	Check    bool
	Provider string
	Model    string
	Asked    bool // the user named a model; otherwise launch leaves the choice to /model and the aliases
	NoBeta   bool // the provider's own Env carries the beta opt-out, so the verb does not inject it
	Rest     []string
}

// spawn is a package var so tests never launch the real CLI.
var spawn = proc.RunForwarding

// Run is the `claude-code databricks` verb; args follow the verb name and it returns the exit code.
func Run(args []string) int {
	if err := helptext.RemovedFlagHint(args); err != nil {
		fmt.Fprintf(os.Stderr, "claude-code databricks: %v\n", err)
		return 2
	}
	if code, ok := helpExit(args); ok {
		return code
	}
	if code, ok := subverbExit(args); ok {
		return code
	}
	p, err := plan(context.Background(), args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code databricks: %v\n", err)
		return 2
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code databricks: cannot locate the binary: %v\n", err)
		return 2
	}
	if p.Check {
		return check(self, p)
	}
	return launch(self, p)
}

// helpExit handles the usage and docs requests, returning the exit code and whether it handled args.
func helpExit(args []string) (int, bool) {
	// Help only as the first arg (or right after check); a later -h belongs to claude, as in the shell wrapper.
	head := args
	if len(head) > 0 && head[0] == "check" {
		head = head[1:]
	}
	if len(head) > 0 && helptext.WantsHelp(head[:1]) {
		usage(len(args) > len(head))
		return 0, true
	}
	if len(args) == 1 && args[0] == helptext.DocsFlag {
		return helptext.WrappedDocs("databricks"), true
	}
	return 0, false
}

// subverbExit runs a `databricks <verb>` or `check --all` command, returning its exit code and whether args named one.
func subverbExit(args []string) (int, bool) {
	if len(args) > 0 {
		if run, ok := subverbs[args[0]]; ok {
			return run(args[1:]), true
		}
	}
	if len(args) > 1 && args[0] == "check" {
		if rest, ok := cutFlag(args[1:], "--all"); ok {
			return checkAll(rest), true
		}
	}
	return 0, false
}

// cutFlag returns args without the first occurrence of flag, and whether it was present.
func cutFlag(args []string, flag string) ([]string, bool) {
	for i, a := range args {
		if a == flag {
			return append(append([]string{}, args[:i]...), args[i+1:]...), true
		}
	}
	return args, false
}

// subverbs are the `databricks <verb>` commands; anything else is a launch (or check) of Claude Code.
var subverbs = map[string]func([]string) int{
	"config": config, "rescue": rescue, "login": login, "token": mint, "import": importCfg, "export": export,
}

// plan parses args: optional leading check, optional MODEL, --profile; everything else passes through.
func plan(ctx context.Context, args []string) (launchPlan, error) {
	p, name, err := parsePlanArgs(args)
	if err != nil {
		return launchPlan{}, err
	}
	reg, err := providers.Load()
	if err != nil {
		return launchPlan{}, fmt.Errorf("load providers: %w", err)
	}
	if p.Provider = resolveProfile(ctx, reg, name); p.Provider == "" {
		return launchPlan{}, errNoProfile
	}
	if !slices.Contains(providers.DatabricksProfiles(reg), p.Provider) {
		return launchPlan{}, fmt.Errorf("--profile %q is not a Databricks profile", p.Provider)
	}
	if p.Model == "" {
		p.Model = defaultModel(reg, p.Provider)
	}
	p.NoBeta = providerHasBetaOff(reg, p.Provider)
	return p, nil
}

// errEmptyProfile refuses an explicit --profile with no name rather than substituting the default profile.
var errEmptyProfile = errors.New("--profile needs a profile name; drop the flag to use the default profile")

// parsePlanArgs splits args into the launchPlan fields they set and the raw --profile name.
func parsePlanArgs(args []string) (launchPlan, string, error) {
	var p launchPlan
	if len(args) > 0 && args[0] == "check" {
		p.Check = true
		args = args[1:]
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		p.Model, p.Asked = args[0], true
		args = args[1:]
	}
	return scanPlanFlags(p, args)
}

// scanPlanFlags takes --profile out of args; every other arg, and everything after "--", goes to p.Rest.
func scanPlanFlags(p launchPlan, args []string) (launchPlan, string, error) {
	name := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--" { // the child's end-of-options marker: keep it and everything after for the child
			p.Rest = append(p.Rest, args[i:]...)
			return p, name, nil
		}
		n, used, ok := profileArg(args, i)
		if !ok {
			p.Rest = append(p.Rest, args[i])
			continue
		}
		if n == "" {
			return p, "", errEmptyProfile
		}
		name, i = n, i+used-1
	}
	return p, name, nil
}

// profileArg reads a --profile/-p flag at args[i] (-p is --profile here, not print mode): name, args spanned, ok.
func profileArg(args []string, i int) (name string, used int, ok bool) {
	switch arg := args[i]; {
	case arg == "--profile", arg == "-p":
		if i+1 < len(args) {
			return args[i+1], 2, true
		}
		return "", 1, true
	case strings.HasPrefix(arg, "--profile="), strings.HasPrefix(arg, "-p="):
		return arg[strings.Index(arg, "=")+1:], 1, true
	}
	return "", 0, false
}

// defaultModel is the opus alias spelled the way the profile's row serves it.
func defaultModel(reg *providers.Registry, profile string) string {
	id := aliasID(models.Opus)
	if reg != nil && reg.Providers[profile] != nil {
		return reg.Providers[profile].WireModel(id)
	}
	return id
}

// errNoProfile is what a verb reports when resolveProfile finds no Databricks profile to default to.
var errNoProfile = errors.New("no Databricks profile; import ~/.databrickscfg with `claude-code databricks import --apply`, or pass --profile")

// resolveProfile is name, else the session, last or recorded claude profile (if Databricks), else the first one.
func resolveProfile(ctx context.Context, reg *providers.Registry, name string) string {
	if name != "" || reg == nil {
		return name
	}
	profs := providers.DatabricksProfiles(reg)
	// The session profile shadows the vault pin, even when it is not a Databricks profile.
	if s := providers.SessionProfileContext(ctx); s != "" {
		if slices.Contains(profs, s) {
			return s
		}
	} else {
		for _, c := range []string{providers.LastProfile("claude"), providers.AgentRecord("claude").Profile} {
			if slices.Contains(profs, c) {
				return c
			}
		}
	}
	if len(profs) > 0 {
		return profs[0]
	}
	return ""
}

// providerHasBetaOff reports whether the profile's row Env already sets the experimental-beta opt-out.
func providerHasBetaOff(reg *providers.Registry, name string) bool {
	if reg == nil {
		return false
	}
	pr, ok := reg.Providers[name]
	if !ok || pr == nil {
		return false
	}
	_, ok = pr.Env[providers.BetaOffKey]
	return ok
}

// childEnv is the parent environment plus the beta opt-out, unless the provider's own Env already carries it.
func childEnv(p launchPlan) []string {
	if p.NoBeta {
		return os.Environ()
	}
	return append(os.Environ(), betaOff)
}

// launch execs this binary as `--profile P --model M rest...` with the parent's stdio.
func launch(self string, p launchPlan) int {
	argv := []string{"--profile", p.Provider}
	if p.Asked {
		argv = append(argv, "--model", p.Model)
	}
	argv = append(argv, p.Rest...)
	code, err := spawn(self, argv, childEnv(p), os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code databricks: exec failed: %v\n", err)
		return 2
	}
	return code
}

// checkTimeout bounds one smoke call so a hung endpoint cannot stall `check` (or a --all sweep).
const checkTimeout = 2 * time.Minute

// spawnCtx is spawn with a deadline: the child is killed when ctx ends; a package var so tests never launch it.
var spawnCtx = func(ctx context.Context, bin string, argv, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, stdin, stdout, stderr
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode(), nil
	}
	return 0, err
}

// capture runs one prompt through the provider: combined output and exit code, -1 with the error on start failure.
func capture(self string, p launchPlan, prompt string, extra ...string) (string, int) {
	var buf bytes.Buffer
	argv := append([]string{"--profile", p.Provider, "--model", p.Model, "-p", prompt}, extra...)
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()
	code, err := spawnCtx(ctx, self, argv, childEnv(p), nil, &buf, &buf)
	if err != nil {
		fmt.Fprint(&buf, err)
		code = -1
	} else if ctx.Err() != nil {
		fmt.Fprintf(&buf, "\ntimed out after %s", checkTimeout)
		code = -1
	}
	return buf.String(), code
}

// check runs the smoke reply; exit 0 only if the child exits 0 and its last line is OK.
func check(self string, p launchPlan) int {
	rc := 0
	fmt.Printf("== smoke: %s %s\n", p.Provider, p.Model)
	if out, code := capture(self, p, smokePrompt, p.Rest...); code == 0 && textutil.LastLine(out) == "OK" {
		fmt.Println("PASS smoke")
	} else {
		fmt.Printf("FAIL smoke: %s\n", tail(out, 5))
		rc = 1
	}
	return rc
}

// tail returns the last n lines of s, trailing newlines dropped.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// page is the `-h` text; its FLAGS rows come from the FlagSet built in usage.
var page = helptext.Page{
	Usage: "claude-code databricks [check|<command>] [MODEL] [flags] [args...]",
	About: "Launch Claude Code on a Databricks profile. MODEL (optional) picks the model; check runs a smoke reply.",
	Commands: [][2]string{
		{"check", "--all: one tiny call per profile (serving; --wire all)"},
		{"login", "sign in: browser for oauth, a mint for an sp row"},
		{"token", "fresh token, never printed; --copy, --auth kind"},
		{"import", "import a .databrickscfg (dry-run unless --apply)"},
		{"export", "export profiles as .databrickscfg, no secrets"},
		{"config", "<target>: render a claude or codex config (never a token)"},
		{"rescue", "break-glass: run claude on a scratch settings file"},
	},
	Hint: "claude-code databricks <command> -h lists that command's flags.\n" +
		"claude-code databricks " + helptext.DocsFlag + " prints the databricks CLI's own help.",
}

// checkPage is `databricks check -h`.
var checkPage = helptext.Page{
	Usage: "claude-code databricks check [MODEL] [--all [--wire W]] [--profile NAME]",
	About: "Smoke-test a Databricks profile with one tiny reply; --all tries every profile on the " +
		"serving wire (--wire ai-gateway|all for AI Gateway).",
}

// usage prints the verb's help; the FlagSet only documents flags, plan does the parsing.
func usage(check bool) {
	pg := page
	if check {
		pg = checkPage
	}
	fs := helptext.NewFlagSet("claude-code databricks", pg)
	profileFlagVar(fs, new(string), profileUsage)
	if check {
		fs.Bool("all", false, "probe every Databricks profile, one tiny call each")
		fs.String("model", aliasID(models.Haiku), "with --all: canonical model id to probe")
		fs.String("wire", providers.WireServing, "with --all: serving, ai-gateway or all")
	}
	fs.Usage()
}

const (
	probeTimeout   = 15 * time.Second // one max_tokens=1 ping
	profileTimeout = 20 * time.Second // one profile's token plus its pings
	checkWorkers   = 3                // profiles probed at once
	budgetDenied   = "budget denied"
)

var checkAllPage = helptext.Page{
	Usage: "claude-code databricks check --all [flags]",
	About: "One tiny Anthropic call per profile on the serving wire (--wire all adds\n" +
		"ai-gateway); prints a status per cell, never a token. Exit 1 when a cell\n" +
		"fails for any reason other than a budget denial.",
}

// probeClient is the HTTP client for probes; serving.ProbeOnce bounds each request.
var probeClient = &http.Client{}

// checkWires maps --wire to the wires probed and their column headers.
func checkWires(wire string) ([]string, []string, error) {
	switch wire {
	case providers.WireServing:
		return []string{providers.WireServing}, []string{"SERVING"}, nil
	case providers.WireAIGateway:
		return []string{providers.WireAIGateway}, []string{"AI-GATEWAY"}, nil
	case "all":
		return []string{providers.WireServing, providers.WireAIGateway}, []string{"SERVING", "AI-GATEWAY"}, nil
	}
	return nil, nil, fmt.Errorf("--wire must be serving, ai-gateway or all (got %q)", wire)
}

// checkRow is one profile's probe outcome.
type checkRow struct {
	cells         []string
	reason, notes string
}

// checkAll is `databricks check --all`: the profile x wire matrix (one profile with --profile).
func checkAll(args []string) int {
	fs := helptext.NewFlagSet("databricks check --all", checkAllPage)
	model := fs.String("model", aliasID(models.Haiku), "canonical model id to probe")
	wire := fs.String("wire", providers.WireServing, "serving, ai-gateway or all")
	only := new(string)
	profileFlagVar(fs, only, "probe only this Databricks profile (default: all)")
	rest, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(rest) > 1 {
		fs.Usage()
		return 2
	}
	if err := checkProfileFlag(fs, *only); err != nil {
		fail("check: %v", err)
		return 2
	}
	if len(rest) == 1 { // positional MODEL overrides --model
		*model = rest[0]
	}
	wires, heads, err := checkWires(*wire)
	if err != nil {
		return fail("check: %v", err)
	}
	reg, err := providers.Load()
	if err != nil {
		return fail("check: %v", err)
	}
	names, err := checkNames(reg, *only)
	if err != nil {
		return fail("check: %v", err)
	}
	return printCheck(names, heads, checkProfiles(reg, names, *model, wires))
}

// checkNames is every Databricks profile, or just only when it names one (an unknown name probes as a miss).
func checkNames(reg *providers.Registry, only string) ([]string, error) {
	names := providers.DatabricksProfiles(reg)
	if only == "" {
		return names, nil
	}
	if reg.Providers[only] != nil && !slices.Contains(names, only) {
		return nil, fmt.Errorf("--profile %q is not a Databricks profile", only)
	}
	return []string{only}, nil
}

// printCheck prints the table, then reasons and SP notes under it; 1 when a cell failed other than a budget denial.
func printCheck(names, heads []string, rows []checkRow) int {
	fmt.Println(checkLine("PROFILE", heads))
	rc := 0
	var why []string
	for i, name := range names {
		r := rows[i]
		for _, c := range r.cells {
			if c != "ok" && c != budgetDenied {
				rc = 1
			}
		}
		fmt.Println(checkLine(name, r.cells))
		if r.reason != "" {
			why = append(why, fmt.Sprintf("  %s: %s", name, r.reason))
		}
		if r.notes != "" {
			why = append(why, "  "+strings.ReplaceAll(strings.TrimSpace(r.notes), "\n", "\n  "))
		}
	}
	if len(why) > 0 {
		fmt.Println()
		fmt.Println(strings.Join(why, "\n"))
	}
	return rc
}

// checkLine is one table row: the label then each cell in a 24-wide column, the last unpadded.
func checkLine(label string, cells []string) string {
	cols := append([]string{label}, cells...)
	for i := range len(cols) - 1 {
		cols[i] = fmt.Sprintf("%-24s", cols[i])
	}
	return strings.Join(cols, " ")
}

// checkProfiles probes names concurrently, checkWorkers at a time; rows keep the names' order.
func checkProfiles(reg *providers.Registry, names []string, model string, wires []string) []checkRow {
	rows := make([]checkRow, len(names))
	sem := make(chan struct{}, checkWorkers)
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			rows[i] = checkProfile(reg.Providers[name], name, model, wires)
		})
	}
	wg.Wait()
	return rows
}

// checkProfile mints the row's token once and probes every wire with it; reason is the full cause when no wire ran.
func checkProfile(row *providers.Provider, name, model string, wires []string) checkRow {
	r := checkRow{cells: make([]string, len(wires))}
	fill := func(s, why string) checkRow {
		for i := range r.cells {
			r.cells[i] = s
		}
		r.reason = why
		return r
	}
	if row == nil {
		return fill("no row", "not in the registry")
	}
	host := providers.AuthHost(row)
	if host == "" {
		return fill("no https host", "the profile has no https host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), profileTimeout)
	defer cancel()
	var notes noteBuf
	ctx = oauth.WithoutRetry(providers.WithNotes(ctx, notes.add)) // a check reports a 429, it never waits one out
	tok, err := providers.Credential(ctx, row)
	r.notes = notes.String()
	if err != nil {
		return fill("no token", strings.TrimPrefix(err.Error(), name+": "))
	}
	var wg sync.WaitGroup
	for i, w := range wires {
		wg.Go(func() { r.cells[i] = probeWire(ctx, row, name, host, w, tok, model) })
	}
	wg.Wait()
	return r
}

// noteBuf collects credential notes; a detached mint may add from another goroutine.
type noteBuf struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (n *noteBuf) add(s string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.buf.WriteString(s)
	n.buf.WriteString("\n")
}

func (n *noteBuf) String() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.buf.String()
}

// probeWire probes row on wire w, synthesizing the row for a wire other than its own.
func probeWire(ctx context.Context, row *providers.Provider, name, host, w, tok, model string) string {
	p := row
	if w != row.Options.Wire && (w != providers.WireServing || row.Options.Wire != "") {
		var err error
		if p, _, err = synth(name, host, w); err != nil {
			return clip(err.Error(), 40)
		}
	}
	return probe(ctx, p, tok, model)
}

// probe sends one max_tokens=1 message on p's Anthropic route, all tries within probeTimeout, and names the reply.
func probe(ctx context.Context, p *providers.Provider, tok, model string) string {
	base, ok := p.Route(providers.ProtocolAnthropic)
	if !ok {
		return "no anthropic route"
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	rep, err := serving.ProbeOnce(ctx, p, serving.CheckOptions{Client: probeClient, Timeout: probeTimeout}, serving.ProbeCall{
		URL: strings.TrimRight(base, "/") + "/v1/messages", Token: tok, Anthropic: true,
		Body: map[string]any{"model": p.WireModel(model), "max_tokens": 1,
			"messages": []map[string]string{{"role": "user", "content": "ping"}}}})
	switch {
	case err != nil && rep.HTTP != 0: // a truncated body is a failure even on 2xx
		return fmt.Sprintf("HTTP %d (body cut off)", rep.HTTP)
	case err != nil:
		return err.Error()
	case rep.HTTP/100 == 2:
		return "ok"
	case rep.Denied:
		return budgetDenied
	case serving.TransientHTTP(rep.HTTP):
		return clip(serving.StatusTransient+" "+classify(rep.HTTP, rep.Body), 48)
	}
	return clip(classify(rep.HTTP, rep.Body), 48)
}

// classify names a failed reply as HTTP <code>: <message>, the message taken from a JSON error body when there is one.
func classify(status int, body string) string {
	var e struct {
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(body)
	if json.Unmarshal([]byte(body), &e) == nil {
		msg = strings.TrimSpace(e.Error.Message + " " + e.Message)
	}
	if msg == "" {
		return fmt.Sprintf("HTTP %d", status)
	}
	return fmt.Sprintf("HTTP %d: %s", status, msg)
}

// clip cuts s to n runes on one line.
func clip(s string, n int) string {
	return textutil.Truncate(strings.Join(strings.Fields(s), " "), n)
}

// Spec is the completion node for the databricks verb.
func Spec() *helptext.Node {
	flag, boolFlag, valuedFlag := helptext.Plain, helptext.BoolFlag, helptext.ValuedFlag
	profileNames := providers.ProfileNames
	identities := func() []string { return []string{"user", "sp"} }
	return &helptext.Node{Flags: []helptext.Flag{valuedFlag("profile", profileNames)}, Sub: map[string]*helptext.Node{
		"check": {Flags: []helptext.Flag{boolFlag("all"), valuedFlag("profile", profileNames), flag("model"),
			valuedFlag("wire", func() []string { return []string{providers.WireServing, providers.WireAIGateway, "all"} })}},
		"login": {Flags: []helptext.Flag{valuedFlag("profile", profileNames), flag("auth-type"), boolFlag("dry-run"), boolFlag("force"), valuedFlag("as", identities),
			flag("port"), boolFlag("no-browser"), boolFlag("paste")}},
		"token": {Flags: []helptext.Flag{valuedFlag("profile", profileNames), boolFlag("dry-run"), boolFlag("copy"), flag("clear-after"),
			valuedFlag("as", identities), valuedFlag("auth", func() []string { return []string{"oauth", "m2m", "pat"} })}},
		"import": {Flags: []helptext.Flag{flag("dbxcfg"), boolFlag("pick"), flag("rename"), boolFlag("replace"),
			boolFlag("seal-secrets"), boolFlag("apply"), boolFlag("dry-run"), boolFlag("fold-legacy")}},
		"export": {Flags: []helptext.Flag{boolFlag("pick"), flag("rename"), flag("out"), boolFlag("merge"), boolFlag("force")}},
		"config": {Args: []func() []string{func() []string { return []string{"claude", "codex"} }}, Flags: dbxCfgFlags(true)},
		"rescue": {Flags: append(dbxCfgFlags(false), boolFlag("print"))},
	}}
}

// dbxCfgFlags are the flags `databricks config` (withOut) and `databricks rescue` share.
func dbxCfgFlags(withOut bool) []helptext.Flag {
	fs := []helptext.Flag{{Name: "profile", Values: providers.ProfileNames}, {Name: "host"}, {Name: "wire"},
		{Name: "model"}, {Name: "auth"}, {Name: "set"}, {Name: "unset"}}
	if withOut {
		fs = append(fs, helptext.Flag{Name: "out"}, helptext.Flag{Name: "force", IsBool: true})
	}
	return fs
}
