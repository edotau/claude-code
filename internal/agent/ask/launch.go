package ask

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/agent/launchplan"
	"github.com/edotau/claude-code/internal/agent/roles"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/transcript"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// Launch launches the named agent CLI and returns the exit code.
func Launch(name string, args []string) int {
	opts, rest, err := launchplan.ParseLaunchArgs(args, false)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	plan, err := launchplan.LaunchPlan(context.Background(), name, opts, rest)
	if err == nil {
		warnVersion(name, plan.Binary, opts, rest)
		if !opts.PrintEnv && !launchplan.IsHeadless(rest) {
			providers.RecordLastProfile(name, plan.Profile)
		}
	}
	return launchplan.Run(name, plan, opts, err)
}

// warnVersion prints the one-line out-of-bounds warning for an interactive launch of the agent binary at path.
func warnVersion(name, path string, opts agent.LaunchOptions, args []string) {
	if !launchplan.WantsVersionWarning(opts, args) || !agentVersionTTY() {
		return
	}
	if line := tools.VersionWarning(name, path); line != "" {
		fmt.Fprintln(versionWarnOut, line)
	}
}

// agentVersionTTY and versionWarnOut are the version warning's terminal gate and sink (tests swap them).
var (
	agentVersionTTY           = func() bool { return ansi.IsTerminal(os.Stderr) }
	versionWarnOut  io.Writer = os.Stderr
)

// Explore is `<agent> explore`: scan the code (and the web when web is set) for question and return a brief.
func Explore(name, question string, web, background bool, format string) int {
	cwd, _ := os.Getwd()
	askArgs, err := roles.Args(name, cwd, question, web, background)
	if err != nil {
		return helptext.Fail("%s explore: %v", name, err)
	}
	if format != helptext.Text {
		task := askArgs[len(askArgs)-1]
		askArgs = append(askArgs[:len(askArgs)-1], "--format", format, task)
	}
	return Cmd(askArgs)
}

// Transfer is `<agent> transfer`: hand the source transcript (default: this repo's newest) to name with note.
func Transfer(name, source, note string, wait bool) int {
	cwd, _ := os.Getwd()
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
		if rec, _, err := job.JobResult(context.Background(), name, cwd, ""); err == nil {
			PrintResumeHint(name, rec.SessionID)
		}
	}
	return code
}

// PrintResumeHint prints the agent's resume command for a session; nothing when it has none.
func PrintResumeHint(name, sessionID string) {
	s, _ := agent.LookupSchema(name)
	if hint := s.ResumeHint(sessionID); hint != "" {
		fmt.Printf("resume: %s\n", hint)
	}
}

// Catalog is `<agent> catalog`: the agent's own catalog (a ModelsProvider lists through models), or a no-catalog note.
func Catalog(name, profile, format string, models func(args []string) int) int {
	s, ok := agent.LookupSchema(name)
	if !ok || !s.External() {
		return helptext.Fail("unknown agent %q", name)
	}
	if name == "copilot" {
		if done, code := copilotSource(profile, format); done {
			return code
		}
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
		return Launch(name, s.CLI.ModelsArgv)
	}
	fmt.Printf("%s: no model catalog\n", name)
	return 0
}

// copilotSource prints a Databricks profile's catalog (done) or, in text, the GitHub model source line.
func copilotSource(profile, format string) (bool, int) {
	reg, err := providers.Load()
	if err != nil {
		return true, helptext.Fail("%v", err)
	}
	cat, err := copilot.CopilotModelSource(context.Background(), reg, profile)
	if err == nil && cat.Databricks {
		err = cat.Write(os.Stdout, format)
	}
	if err != nil {
		return true, helptext.Fail("copilot catalog: %v", err)
	}
	if cat.Databricks {
		return true, 0
	}
	if format == helptext.Text {
		fmt.Println(cat.Source)
	}
	return false, 0
}
