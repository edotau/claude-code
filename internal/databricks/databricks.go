// Package databricks launches Claude Code on Databricks model serving and runs the endpoint smoke checks.
package databricks

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
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
	Asked    bool // the user named a model; otherwise launch leaves the choice to /model and the slots
	NoBeta   bool // the provider's own Env carries the beta opt-out, so the verb does not inject it
	Rest     []string
}

// spawn runs bin with argv and env, returning the child's exit code (err only if it could not start).
// A package var so tests never launch the real CLI.
var spawn = func(bin string, argv, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd := exec.Command(bin, argv...)
	cmd.Env = env
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	// The tty already delivers SIGINT to the child; the parent only survives it and forwards SIGTERM.
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		for s := range sigs {
			if s == syscall.SIGTERM {
				_ = cmd.Process.Signal(s)
			}
		}
	}()
	err := cmd.Wait()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode(), nil
	}
	return 0, err
}

// Run is the `claude-code databricks` verb; args follow the verb name and it returns the exit code.
func Run(args []string) int {
	if err := RemovedFlagHint(args); err != nil {
		fmt.Fprintf(os.Stderr, "claude-code databricks: %v\n", err)
		return 2
	}
	// Help only as the first arg (or right after check); a later -h belongs to claude, as in the shell wrapper.
	head := args
	if len(head) > 0 && head[0] == "check" {
		head = head[1:]
	}
	if len(head) > 0 && helptext.WantsHelp(head[:1]) {
		usage(len(args) > len(head))
		return 0
	}
	if len(args) == 1 && args[0] == helptext.DocsFlag {
		return helptext.WrappedDocs("databricks")
	}
	if len(args) > 0 {
		if run, ok := subverbs[args[0]]; ok {
			return run(args[1:])
		}
	}
	if len(args) > 1 && args[0] == "check" && args[1] == "--all" {
		return checkAll(args[2:])
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

// subverbs are the `databricks <verb>` commands; anything else is a launch (or check) of Claude Code.
var subverbs = map[string]func([]string) int{
	"config": config, "rescue": rescue, "login": login, "mint": mint, "import": importCfg, "export": export,
}

// plan parses args: optional leading check, optional MODEL, --profile; everything else passes through.
func plan(args []string) (Plan, error) {
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
	reg, _ := providers.Load() // a load error leaves reg nil: the name is taken as given and the beta opt-out injected
	if p.Provider = resolveProfile(reg, name); p.Provider == "" {
		return Plan{}, errors.New("no Databricks profile; add one with `databricks import` or pass --profile")
	}
	if reg != nil && !slices.Contains(providers.DatabricksProfiles(reg), p.Provider) {
		return Plan{}, fmt.Errorf("--profile %q is not a Databricks profile", p.Provider)
	}
	if p.Model == "" {
		p.Model = defaultModel(reg, p.Provider)
	}
	p.NoBeta = providerHasBetaOff(reg, p.Provider)
	return p, nil
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

// resolveProfile is name, else the session profile, else the vault CLAUDE_PROFILE pin, each only when it is
// a Databricks profile, else the first Databricks profile; "" when there is none.
func resolveProfile(reg *providers.Registry, name string) string {
	if name != "" || reg == nil {
		return name
	}
	profs := providers.DatabricksProfiles(reg)
	// The session profile shadows the vault pin, even when it is not a Databricks profile.
	if c := cmp.Or(providers.SessionProfile(), providers.Pin(providers.PinProfile)); slices.Contains(profs, c) {
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

// capture runs one prompt through the provider and returns combined stdout+stderr (a start error is appended).
func capture(self string, p Plan, prompt string, stdin io.Reader, extra ...string) string {
	var buf bytes.Buffer
	argv := append([]string{"--profile", p.Provider, "--model", p.Model, "-p", prompt}, extra...)
	if _, err := spawn(self, argv, childEnv(p), stdin, &buf, &buf); err != nil {
		fmt.Fprint(&buf, err)
	}
	return buf.String()
}

// check runs the smoke reply; exit 0 only if it passes.
func check(self string, p Plan) int {
	rc := 0
	fmt.Printf("== smoke: %s %s\n", p.Provider, p.Model)
	if out := capture(self, p, smokePrompt, nil); lastLine(out) == "OK" {
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
