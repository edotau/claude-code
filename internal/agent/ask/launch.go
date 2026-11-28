package ask

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/cli/ui/helptext"
	"github.com/edotau/claude-code/internal/cli/ui/tui"
	"github.com/edotau/claude-code/internal/dev/transcript"
	"github.com/edotau/claude-code/internal/driver/gemini"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/vault"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// Launch launches the named agent CLI and returns the exit code.
func Launch(name string, args []string) int {
	opts, rest, err := agent.ParseLaunchArgs(args, false)
	if err != nil {
		return helptext.FailUsage("%v", err)
	}
	plan, err := agent.LaunchPlan(context.Background(), name, opts, rest)
	if err == nil {
		warnVersion(name, plan.Binary, opts, rest)
	}
	return agent.RunPlan(name, plan, opts, err)
}

// vsCodeApp is the bundle `gemini vscode` relaunches; its main process holds the gateway token the extension uses.
const vsCodeApp = "/Applications/Visual Studio Code.app"

// VSCode is `<agent> vscode [launch flags] [code args…]`: a VS Code window, first party unless a route is typed.
func VSCode(name string, here bool, args []string) int {
	if runtime.GOOS != "darwin" {
		return helptext.Fail("%s vscode: macOS only", name)
	}
	opts, codeArgs, err := agent.ParseLaunchArgs(args, false)
	if err != nil {
		return helptext.FailUsage("%v", err)
	}
	if !opts.PrintEnv && !here {
		vsCodeOpenNote(name, codeArgs)
	}
	if !opts.ProfileFlag && opts.Overlay == "" { // first party by design: no route, no pin
		return vsCodeFirstParty(name, codeArgs, opts.PrintEnv)
	}
	return vsCodeRouted(name, here, args, opts, codeArgs)
}

// vsCodeRouted opens VS Code on the typed route: a new window when it runs, else a detached start that becomes it.
func vsCodeRouted(name string, here bool, args []string, opts agent.LaunchOptions, codeArgs []string) int {
	plan, err := agent.LaunchPlan(vsCodeFailover(opts), name, opts, nil)
	if err != nil {
		return helptext.Fail("%s vscode: %v", name, err)
	}
	main := vsCodeMain()
	switch {
	case here || (main == 0 && opts.PrintEnv):
		plan.Binary, plan.Argv = filepath.Join(vsCodeApp, "Contents", "MacOS", "Code"), codeArgs // same PID as the token
	case main == 0: // not running: a detached copy becomes VS Code, so this terminal may close
		return vsCodeDetach(name, args)
	default: // a new window of the running VS Code; its extension host takes the CLI's env
		if err := vsCodeWindow(&plan, main, opts.PrintEnv, codeArgs); err != nil {
			return helptext.Fail("%s vscode: %v", name, err)
		}
	}
	return agent.RunPlan(name, plan, opts, nil)
}

// vsCodeFailover lets the window's token move to a sibling profile of its provider on a 429: an IDE cannot re-route.
func vsCodeFailover(opts agent.LaunchOptions) context.Context {
	ctx := context.Background()
	reg, err := providers.Load()
	if err != nil {
		return ctx
	}
	if row := reg.Row(cmp.Or(opts.Route.Profile, opts.Provider)); row != nil {
		ctx = agent.WithAutoFailover(ctx, row.Provider)
	}
	return ctx
}

// vsCodeFirstParty opens `code --new-window` on the vendor login, dropping a gateway env a routed window passed on.
func vsCodeFirstParty(name string, codeArgs []string, dry bool) int {
	bin := filepath.Join(vsCodeApp, "Contents", "Resources", "app", "bin", "code")
	argv := append([]string{"--new-window"}, codeArgs...)
	if dry {
		fmt.Printf("binary   %s\nargv     %s\n", bin, strings.Join(argv, " "))
		return 0
	}
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "AGY_LLM_GATEWAY_") })
	return helptext.Fail("%s vscode: exec %s: %v", name, bin, proc.Exec(bin, argv, env))
}

// vsCodeWindow points plan at `code --new-window`, its gateway token re-signed to VS Code's main process main.
func vsCodeWindow(plan *agent.Plan, main int, dry bool, codeArgs []string) error {
	if plan.Set["AGY_LLM_GATEWAY_API_KEY"] != "" && !dry {
		if err := gemini.RebindGatewayToken(plan.Set, main); err != nil {
			return err
		}
	}
	plan.Binary = filepath.Join(vsCodeApp, "Contents", "Resources", "app", "bin", "code")
	plan.Argv = append([]string{"--new-window"}, codeArgs...)
	return nil
}

// vsCodeOpenNote warns that VS Code only focuses an open folder: two windows on one repo run two agents on it.
func vsCodeOpenNote(name string, codeArgs []string) {
	if slices.ContainsFunc(codeArgs, func(a string) bool { st, err := os.Stat(a); return err == nil && st.IsDir() }) {
		fmt.Fprintf(os.Stderr, "%s vscode: a folder already open keeps its window and route; close it first to "+
			"change the route\n", name)
	}
}

// vsCodeMain is the running VS Code's main process id, 0 when VS Code is not running (pgrep misses it on macOS).
func vsCodeMain() int {
	bin := filepath.Join(vsCodeApp, "Contents", "MacOS", "Code")
	out, err := exec.CommandContext(context.Background(), "ps", "-axo", "pid=,comm=").Output()
	if err != nil {
		return 0
	}
	for line := range strings.Lines(string(out)) {
		pid, comm, _ := strings.Cut(strings.TrimSpace(line), " ")
		if strings.TrimSpace(comm) == bin {
			n, _ := strconv.Atoi(pid)
			return n
		}
	}
	return 0
}

// vsCodeDetach reruns `<agent> vscode --here` in its own session, logging to the state dir.
func vsCodeDetach(name string, args []string) int {
	self, err := os.Executable()
	if err != nil {
		return helptext.Fail("%s vscode: %v", name, err)
	}
	log := filepath.Join(paths.StateDir(), name+"-vscode.log")
	f, err := os.OpenFile(log, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return helptext.Fail("%s vscode: %v", name, err)
	}
	defer f.Close()
	if !proc.SpawnDetached(self, append([]string{name, "vscode", "--here"}, args...), "", f) {
		return helptext.Fail("%s vscode: could not start VS Code", name)
	}
	fmt.Printf("%s vscode: starting VS Code; log %s\n", name, log)
	return 0
}

// warnVersion prints the one-line out-of-bounds warning for an interactive launch of the agent binary at path.
func warnVersion(name, path string, opts agent.LaunchOptions, args []string) {
	if !agent.WantsVersionWarning(opts, args) || !agentVersionTTY() {
		return
	}
	if line := tools.VersionWarning(name, path); line != "" {
		fmt.Fprintln(versionWarnOut, line)
	}
}

// agentVersionTTY and versionWarnOut are the version warning's terminal gate and sink (tests swap them).
var (
	agentVersionTTY           = func() bool { return proc.IsTerminal(os.Stderr) }
	versionWarnOut  io.Writer = os.Stderr
)

// Transfer is `<agent> transfer`: hand the source transcript (default: this repo's newest) to name with note.
func Transfer(name, source, note string, wait bool) int {
	cwd, err := os.Getwd()
	if err != nil {
		return helptext.Fail("%s transfer: %v", name, err)
	}
	src, err := transcript.ResolveSource(source, cwd)
	if err != nil {
		return helptext.Fail("%s transfer: %v", name, err)
	}
	fmt.Fprintf(os.Stderr, "transfer: source %s\n", src)
	prompt, err := transcript.Handoff(src, cwd, note)
	if err != nil {
		return helptext.Fail("%s transfer: %v", name, err)
	}
	code := Cmd(transcript.TransferArgs(name, cwd, prompt, wait))
	if code == 0 && wait {
		if rec, _, err := agent.JobResult(context.Background(), name, cwd, ""); err == nil {
			fprintResumeHint(os.Stdout, name, rec.SessionID)
		}
	}
	return code
}

// fprintResumeHint prints the agent's resume command for a session to w; nothing when it has none.
func fprintResumeHint(w io.Writer, name, sessionID string) {
	s, _ := agent.LookupSchema(name)
	if hint := s.ResumeHint(sessionID); hint != "" {
		fmt.Fprintf(w, "resume: %s\n", hint)
	}
}

// Catalog is bare `<agent> catalog` off Databricks: the agent's own list, behind profile when given, else a note.
func Catalog(name, format, profile string, models func(args []string) int) int {
	s, ok := agent.LookupSchema(name)
	if !ok || !s.External() {
		return helptext.Fail("unknown agent %q", name)
	}
	if s.CLI.CatalogTitle != "" && format == helptext.Text {
		fmt.Println(s.CLI.CatalogTitle)
	}
	switch {
	case s.CLI.ModelsProvider != "":
		reg, err := providers.Load()
		if err != nil {
			return helptext.Fail("%v", err)
		}
		n, err := s.ModelsProfile(reg)
		if err != nil {
			return helptext.Fail("%v", err)
		}
		return models([]string{"--live", "--profile", n, "--format", format})
	case len(s.CLI.ModelsArgv) > 0:
		return Launch(name, modelsLaunchArgs(s.CLI.ModelsArgv, profile))
	}
	fmt.Printf("%s: no model catalog\n", name)
	return 0
}

// modelsLaunchArgs is the launch argv of a models listing: argv, behind --profile when one was asked for.
func modelsLaunchArgs(argv []string, profile string) []string {
	if profile == "" {
		return argv
	}
	return append([]string{"--profile", profile}, argv...)
}

// WireUI installs terminal and secret prompts; cli wires the interactive Databricks login.
func WireUI() {
	agent.StdinIsTerminal = tui.StdinIsTerminal
	agent.PromptSecret = func(name string) (string, error) {
		return tui.Secret("Paste the value for "+name+" (input hidden; empty cancels): ", vault.Preview)
	}
}

// takeoverBrief is the fixed Pi takeover brief: agent, cutoff, the original task, the partial output.
func takeoverBrief(agentName, cut, task string, n int, partial string) string {
	return assets.TakeoverBrief(agentName, cut, task, n, partial)
}
