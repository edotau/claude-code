// Package databricks launches Claude Code on Databricks model serving and runs the endpoint smoke checks.
package databricks

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

const (
	smokePrompt = "Reply with exactly: OK"
	betaKey     = providers.BetaOffKey
	betaOff     = betaKey + "=1"
)

// Plan is one parsed invocation: the profile (Provider) and model the child launches with, plus pass-through args.
type Plan struct {
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
	if err := RemovedFlagHint(args); err != nil {
		fmt.Fprintf(os.Stderr, "claude-code databricks: %v\n", err)
		return 2
	}
	if code, ok := helpExit(args); ok {
		return code
	}
	if code, ok := subverbExit(args); ok {
		return code
	}
	p, err := plan(args)
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
	if len(args) > 1 && args[0] == "check" && args[1] == "--all" {
		return checkAll(args[2:]), true
	}
	return 0, false
}

// subverbs are the `databricks <verb>` commands; anything else is a launch (or check) of Claude Code.
var subverbs = map[string]func([]string) int{
	"config": config, "rescue": rescue, "login": login, "mint": mint, "import": importCfg, "export": export,
}

// plan parses args: optional leading check, optional MODEL, --profile; everything else passes through.
func plan(args []string) (Plan, error) {
	p, name := parsePlanArgs(args)
	reg, err := providers.Load()
	if err != nil {
		return Plan{}, fmt.Errorf("load providers: %w", err)
	}
	if p.Provider = resolveProfile(reg, name); p.Provider == "" {
		return Plan{}, errNoProfile
	}
	if !slices.Contains(providers.DatabricksProfiles(reg), p.Provider) {
		return Plan{}, fmt.Errorf("--profile %q is not a Databricks profile", p.Provider)
	}
	if p.Model == "" {
		p.Model = defaultModel(reg, p.Provider)
	}
	p.NoBeta = providerHasBetaOff(reg, p.Provider)
	return p, nil
}

// parsePlanArgs splits args into the Plan fields they set and the raw --profile name.
func parsePlanArgs(args []string) (Plan, string) {
	var p Plan
	if len(args) > 0 && args[0] == "check" {
		p.Check = true
		args = args[1:]
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		p.Model, p.Asked = args[0], true
		args = args[1:]
	}
	name := ""
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--profile", arg == "-p": // -p is --profile here; claude's print mode is --print
			name = ""
			if i+1 < len(args) {
				name = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--profile="), strings.HasPrefix(arg, "-p="):
			name = arg[strings.Index(arg, "=")+1:]
		default:
			p.Rest = append(p.Rest, arg)
		}
	}
	return p, name
}

// defaultModel is the opus alias, at 1M where the endpoint serves it, spelled the way the profile's row serves it.
func defaultModel(reg *providers.Registry, profile string) string {
	id := aliasID(models.Opus)
	if reg != nil && reg.Providers[profile] != nil {
		pr := reg.Providers[profile]
		return pr.WireModel(id)
	}
	return id
}

// errNoProfile is what a verb reports when resolveProfile finds no Databricks profile to default to.
var errNoProfile = errors.New("no Databricks profile; import ~/.databrickscfg with `claude-code databricks import --apply`, or pass --profile")

// resolveProfile is name, else the session profile, else the claude record profile, each only when it is
// a Databricks profile, else the first Databricks profile; "" when there is none.
func resolveProfile(reg *providers.Registry, name string) string {
	if name != "" || reg == nil {
		return name
	}
	profs := providers.DatabricksProfiles(reg)
	// The session profile shadows the vault pin, even when it is not a Databricks profile.
	if c := cmp.Or(providers.SessionProfile(), providers.AgentRecord("claude").Profile); slices.Contains(profs, c) {
		return c
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
func childEnv(p Plan) []string {
	if p.NoBeta {
		return os.Environ()
	}
	return append(os.Environ(), betaOff)
}

// launch execs this binary as `--profile P --model M rest...` with the parent's stdio.
func launch(self string, p Plan) int {
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

// capture runs one prompt through the provider and returns combined stdout+stderr and the exit code;
// a start error or timeout is appended to the output with code -1.
func capture(self string, p Plan, prompt string, stdin io.Reader, extra ...string) (string, int) {
	var buf bytes.Buffer
	argv := append([]string{"--profile", p.Provider, "--model", p.Model, "-p", prompt}, extra...)
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()
	code, err := spawnCtx(ctx, self, argv, childEnv(p), stdin, &buf, &buf)
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
func check(self string, p Plan) int {
	rc := 0
	fmt.Printf("== smoke: %s %s\n", p.Provider, p.Model)
	if out, code := capture(self, p, smokePrompt, nil); code == 0 && lastLine(out) == "OK" {
		fmt.Println("PASS smoke")
	} else {
		fmt.Printf("FAIL smoke: %s\n", tail(out, 5))
		rc = 1
	}
	return rc
}

// lastLine returns the last non-empty line of s, trimmed.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, " \t\r\n"), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
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
	About: "Launch Claude Code on a Databricks profile. MODEL defaults to the opus alias at 1M context. " +
		"check runs a smoke reply.",
	Commands: [][2]string{
		{"check", "--all: one tiny call per profile x wire"},
		{"login", "browser sign-in for an oauth profile"},
		{"mint", "fresh token, never printed; --copy to clipboard"},
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
	Usage: "claude-code databricks check [MODEL] [--all] [--profile NAME]",
	About: "Smoke-test a Databricks profile with one tiny reply; --all tries every profile x wire " +
		"(serving/ai-gateway).",
}

// usage prints the verb's help; the FlagSet only documents flags, plan does the parsing.
func usage(check bool) {
	pg := page
	if check {
		pg = checkPage
	}
	fs := helptext.NewFlagSet("claude-code databricks", pg)
	profileFlag(fs)
	fs.Usage()
}

// removedFlags maps each flag Phase 6 removed to its hint; they stay errors, never aliases.
var removedFlags = map[string]string{
	"--provider": "was renamed to --profile",
	"--env":      "was renamed to --profile",
	"--tier":     "was replaced by --model <alias>",
	"--router":   "was removed; routing is automatic (see `claude-code gateway router`)",
	"--1m":       "was removed; 1M context is the base behavior on serving and first-party endpoints",
}

// RemovedFlagHint returns the error for a removed flag in args (up to "--"), or nil.
func RemovedFlagHint(args []string) error {
	for _, a := range args {
		if a == "--" {
			return nil
		}
		name, _, _ := strings.Cut(a, "=")
		if hint, ok := removedFlags[name]; ok {
			return fmt.Errorf("%s %s", name, hint)
		}
	}
	return nil
}
