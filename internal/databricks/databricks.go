// Package databricks launches Claude Code on Databricks model serving and runs the endpoint smoke checks.
package databricks

import (
	"bytes"
	"encoding/json"
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
	"github.com/edotau/claude-code/internal/providers"
)

const (
	defaultModel = "databricks-claude-opus-5-5[1m]"
	defaultEnv   = "qa"
	smokePrompt  = "Reply with exactly: OK"
	needlePrompt = "What is the secret word at the very end of this input? Reply with the word only."
	needleWord   = "ZEBRAFINCH"
	needleText   = "\nThe secret word is ZEBRAFINCH.\n"
	filler       = "The quick brown fox jumps over the lazy dog near riverbank.\n"
	fillerBytes  = 1_200_000
	betaKey      = providers.BetaOffKey
	betaOff      = betaKey + "=1"
)

// Plan is one parsed invocation: the provider and model the child launches with, plus pass-through args.
type Plan struct {
	Check    bool
	Big      bool
	Provider string
	Model    string
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
	// Help only as the first arg (or right after check); a later -h belongs to claude, as in the shell wrapper.
	head := args
	if len(head) > 0 && head[0] == "check" {
		head = head[1:]
	}
	if len(head) > 0 && helptext.WantsHelp(head[:1]) {
		usage()
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
	"agents": agents, "config": config, "rescue": rescue, "login": login, "mint": mint, "import": importCfg, "export": export,
}

// rowName is the provider row for env: the migrated <env> preset row, else the legacy databricks-<env>.
func rowName(reg *providers.Registry, env string) string {
	if reg != nil {
		if p := reg.Providers[env]; p != nil && p.Preset == providers.PresetDatabricks {
			return env
		}
	}
	return "databricks-" + env
}

// plan parses args: optional leading check, optional MODEL, --env, --1m; everything else passes through.
func plan(args []string) (Plan, error) {
	p := Plan{Model: defaultModel}
	if len(args) > 0 && args[0] == "check" {
		p.Check = true
		args = args[1:]
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		p.Model = args[0]
		args = args[1:]
	}
	env := defaultEnv
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--env":
			env = ""
			if i+1 < len(args) {
				env = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--env="):
			env = strings.TrimPrefix(arg, "--env=")
		case arg == "--1m":
			p.Big = true
		default:
			p.Rest = append(p.Rest, arg)
		}
	}
	if !slices.Contains(providers.DatabricksEnvs, env) {
		return Plan{}, fmt.Errorf("--env must be dev, qa or prod (got %q)", env)
	}
	reg, _ := providers.Load() // a load error leaves reg nil: no twin is found and the beta opt-out is injected
	p.Provider = rowName(reg, env)
	if !isClaudeModel(p.Model) && hasProvider(reg, p.Provider+"-oai") {
		p.Provider += "-oai"
	}
	p.NoBeta = providerHasBetaOff(reg, p.Provider)
	return p, nil
}

// hasProvider reports whether reg defines the named provider.
func hasProvider(reg *providers.Registry, name string) bool {
	if reg == nil {
		return false
	}
	_, ok := reg.Providers[name]
	return ok
}

// providerHasBetaOff reports whether the provider's Env already sets the experimental-beta opt-out.
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

// agents is `databricks agents on|off|status`: aliases for the profile verbs, run through this binary.
func agents(args []string) int {
	if len(args) == 0 || helptext.WantsHelp(args[:1]) {
		usage()
		return 0
	}
	var argv []string
	switch args[0] {
	case "on":
		env, err := agentsEnv(args[1:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "claude-code databricks: %v\n", err)
			return 2
		}
		reg, _ := providers.Load()
		argv = agentsUse(reg, env)
	case "off":
		argv = []string{"profile", "off"}
	case "status":
		argv = []string{"profile", "show"}
	default:
		fmt.Fprintf(os.Stderr, "claude-code databricks: agents wants on, off or status (got %q)\n", args[0])
		return 2
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code databricks: cannot locate the binary: %v\n", err)
		return 2
	}
	code, err := spawn(self, argv, os.Environ(), os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code databricks: exec failed: %v\n", err)
		return 2
	}
	return code
}

// agentsUse is the `profile use` argv for `agents on`: a legacy databricks-<env> set, else DEFAULT repointed to <env>.
func agentsUse(reg *providers.Registry, env string) []string {
	if reg != nil {
		if _, ok := reg.Profiles.Sets["databricks-"+env]; ok {
			return []string{"profile", "use", "databricks-" + env}
		}
	}
	return []string{"profile", "use", "--account", env, "DEFAULT"}
}

// agentsEnv picks the env: --env, else the one in HARNESS_PROVIDER (<env> or databricks-<env>[-oai]), else the default.
func agentsEnv(args []string) (string, error) {
	env := ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--env" && i+1 < len(args):
			env = args[i+1]
			i++
		case strings.HasPrefix(a, "--env="):
			env = strings.TrimPrefix(a, "--env=")
		default:
			return "", fmt.Errorf("unexpected argument %q", a)
		}
	}
	env = envOrDefault(env)
	if !slices.Contains(providers.DatabricksEnvs, env) {
		return "", fmt.Errorf("--env must be dev, qa or prod (got %q)", env)
	}
	return env, nil
}

// envOrDefault is env, else HARNESS_PROVIDER's env (the only var read; <env> or databricks-<env>), else qa.
func envOrDefault(env string) string {
	if env == "" {
		env = envFromProvider(os.Getenv("HARNESS_PROVIDER"))
	}
	if env == "" {
		env = defaultEnv
	}
	return env
}

// envFromProvider returns <env> from <env> or the legacy databricks-<env>[-oai], else "".
func envFromProvider(name string) string {
	env := name
	if rest, ok := strings.CutPrefix(name, "databricks-"); ok {
		env = strings.TrimSuffix(rest, "-oai")
	}
	if !slices.Contains(providers.DatabricksEnvs, env) {
		return ""
	}
	return env
}

// isClaudeModel reports whether model takes the Anthropic passthrough (else it goes via the router).
func isClaudeModel(model string) bool {
	return slices.Contains([]string{"opus", "sonnet", "haiku"}, model) || strings.HasPrefix(model, "databricks-claude-") ||
		strings.HasPrefix(model, "claude-")
}

// childEnv is the parent environment plus the beta opt-out, unless the provider's own Env already carries it.
func childEnv(p Plan) []string {
	if p.NoBeta {
		return os.Environ()
	}
	return append(os.Environ(), betaOff)
}

// launch execs this binary as `--provider P --model M rest...` with the parent's stdio.
func launch(self string, p Plan) int {
	argv := append([]string{"--provider", p.Provider, "--model", p.Model}, p.Rest...)
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
	argv := append([]string{"--provider", p.Provider, "--model", p.Model, "-p", prompt}, extra...)
	if _, err := spawn(self, argv, childEnv(p), stdin, &buf, &buf); err != nil {
		fmt.Fprint(&buf, err)
	}
	return buf.String()
}

// check runs the smoke reply and, with --1m, the needle-at-the-end probe; exit 0 only if every check passes.
func check(self string, p Plan) int {
	rc := 0
	fmt.Printf("== smoke: %s %s\n", p.Provider, p.Model)
	if out := capture(self, p, smokePrompt, nil); lastLine(out) == "OK" {
		fmt.Println("PASS smoke")
	} else {
		fmt.Printf("FAIL smoke: %s\n", tail(out, 5))
		rc = 1
	}
	if !p.Big {
		return rc
	}
	body := needleBody()
	fmt.Printf("== 1m: ~300K tokens, needle at the end (%d bytes)\n", len(body))
	out := capture(self, p, needlePrompt, strings.NewReader(body), "--output-format", "json")
	text, tokens := parseUsage(out)
	switch {
	case !strings.Contains(text, needleWord):
		fmt.Printf("FAIL 1m: needle not found: %s\n", tail(out, 5))
		rc = 1
	case tokens <= bigTokens:
		fmt.Printf("FAIL 1m: input_tokens=%d, want > %d (the 1M window is not in effect)\n", tokens, bigTokens)
		rc = 1
	default:
		fmt.Printf("1m PASS (input_tokens=%d)\n", tokens)
	}
	return rc
}

// bigTokens is the standard context window; a real 1M path must report more input tokens than this.
const bigTokens = 200_000

// parseUsage reads claude's --output-format json result: the reply and input tokens (cache create/read included).
func parseUsage(out string) (text string, tokens int) {
	var raw struct {
		Result string `json:"result"`
		Usage  struct {
			In     int `json:"input_tokens"`
			Create int `json:"cache_creation_input_tokens"`
			Read   int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(lastLine(out)), &raw) != nil {
		return out, 0
	}
	return raw.Result, raw.Usage.In + raw.Usage.Create + raw.Usage.Read
}

// needleBody is fillerBytes of repeated sentence followed by the needle line.
func needleBody() string {
	n := fillerBytes/len(filler) + 1
	return strings.Repeat(filler, n)[:fillerBytes] + needleText
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
	About: "Launch Claude Code on Databricks model serving. MODEL defaults to databricks-claude-opus-5-5[1m]. " +
		"opus, sonnet, haiku and databricks-claude-* use the Anthropic passthrough (row <env>, or a legacy " +
		"databricks-<env>); other endpoints go through the router (a databricks-<env>-oai twin, else the row's " +
		"openai route). check runs a smoke reply; with --1m it also runs a ~1.2 MB needle-at-the-end probe.",
	Commands: [][2]string{
		{"check", "--all: one tiny call per env x wire (serving/gateway)"},
		{"login", "browser sign-in for a u2m row; seals the refresh token"},
		{"mint", "fresh token, never printed; --copy to clipboard"},
		{"import", "create rows from ~/.databrickscfg (dry-run unless --apply)"},
		{"export", "write u2m/m2m/pat rows as .databrickscfg sections, no secrets"},
		{"config", "<target>: render a claude or codex config (never a token)"},
		{"rescue", "break-glass: run claude on a scratch settings file"},
		{"agents", "on|off|status: profile use DEFAULT / profile off / show"},
	},
	Hint: "claude-code databricks <command> -h lists that command's flags.\n" +
		"claude-code databricks " + helptext.DocsFlag + " prints the databricks CLI's own help.",
}

// usage prints the verb's help; the FlagSet only documents flags, plan does the parsing.
func usage() {
	fs := helptext.NewFlagSet("claude-code databricks", page)
	fs.String("env", defaultEnv, "Databricks workspace: dev, qa or prod")
	fs.Bool("1m", false, "with check: also run the 1M-context probe")
	fs.Usage()
}
