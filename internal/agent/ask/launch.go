package ask

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/roles"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/transcript"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// Launch launches the named agent CLI and returns the exit code.
func Launch(name string, args []string) int {
	opts, rest, err := agent.ParseLaunchArgs(args, false)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	plan, err := agent.LaunchPlan(context.Background(), name, opts, rest)
	if err == nil {
		warnVersion(name, plan.Binary, opts, rest)
		if !opts.PrintEnv && !agent.IsHeadless(rest) {
			providers.RecordLastProfile(name, plan.Profile)
		}
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

// Explore is `<agent> explore`: scan the code (and the web when web is set) for question and return a brief.
func Explore(name, question string, web, background bool, format string) int {
	cwd, err := os.Getwd()
	if err != nil {
		return helptext.Fail("%s explore: %v", name, err)
	}
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

// Catalog is bare `<agent> catalog` off Databricks: the agent's own catalog via models or ModelsArgv, else a note.
func Catalog(name, format string, models func(args []string) int) int {
	s, ok := agent.LookupSchema(name)
	if !ok || !s.External() {
		return helptext.Fail("unknown agent %q", name)
	}
	if name == "copilot" && format == helptext.Text {
		fmt.Println("GitHub Copilot catalog")
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

// Setup is `<agent> setup`: check name's binary, login, route and projection; exit 1 when one fails.
func Setup(name, format string) int {
	row, ok := agent.LookupSchema(name)
	if !ok {
		row = agent.Schema{Name: name} // reported as an unknown agent, by name
	}
	rep := row.Setup()
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, rep); err != nil {
			return helptext.Fail("%s setup: %v", name, err)
		}
	} else {
		for _, c := range rep.Checks {
			fmt.Printf("%s  %s  %s  %s\n", rep.Agent, c.Name, c.State, c.Detail)
		}
	}
	if !rep.OK {
		return 1
	}
	return 0
}
