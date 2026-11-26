package ask

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/cli/ui/helptext"
	"github.com/edotau/claude-code/internal/cli/ui/tui"
	"github.com/edotau/claude-code/internal/dev/transcript"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/vault"
	"github.com/edotau/claude-code/internal/routing/databricks"
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

// WireUI installs the terminal and sign-in UI the agent package calls but does not import.
func WireUI() {
	agent.StdinIsTerminal = tui.StdinIsTerminal
	agent.LoginRow = databricks.LoginRow
	agent.PromptSecret = func(name string) (string, error) {
		return tui.Secret("Paste the value for "+name+" (input hidden; empty cancels): ", vault.Preview)
	}
}

// takeoverBrief is the fixed Pi takeover brief: agent, cutoff, the original task, the partial output.
func takeoverBrief(agentName, cut, task string, n int, partial string) string {
	return assets.TakeoverBrief(agentName, cut, task, n, partial)
}
