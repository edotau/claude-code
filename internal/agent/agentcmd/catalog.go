package agentcmd

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// ModelsVerb is the models verb (internal/cli wires it); a catalog lists a ModelsProvider agent's models through it.
var ModelsVerb func(args []string) int

func agentCatalog(name string, args []string) int {
	if opencode.IsMenuVerb(name, args) {
		return opencode.RunMenu(args)
	}
	if agentSubHelp(name, "catalog", args) {
		return 0
	}
	return agentModels(name, args)
}

// agentModels is `claude-code <v> catalog` : the agent's own catalog, or a note that it has none.
func agentModels(name string, args []string) int {
	s, ok := agent.LookupSchema(name)
	if !ok || !s.External() {
		return helptext.Fail("unknown agent %q", name)
	}
	format := helptext.Text
	if name == "copilot" {
		var code int
		var done bool
		format, code, done = copilotCatalog(args)
		if done {
			return code
		}
	} else if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "error: %s catalog: unexpected argument %q\n", name, args[0])
		return 2
	}
	return launchAgentCatalog(name, s, format)
}

func copilotCatalog(args []string) (string, int, bool) {
	name := "copilot"
	fs := newAgentFlags(name, "catalog")
	profile := fs.String("profile", "", "list this profile's models (default: the session profile)")
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return "", code, true
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return "", helptext.BadFormat(name+" catalog", err), true
	}
	reg, err := providers.Load()
	if err != nil {
		return "", helptext.Fail("%v", err), true
	}
	cat, err := copilot.CopilotModelSource(context.Background(), reg, *profile)
	if err != nil {
		return "", helptext.Fail("%s catalog: %v", name, err), true
	}
	if cat.Databricks {
		if err := cat.Write(os.Stdout, *format); err != nil {
			return "", helptext.Fail("%s catalog: %v", name, err), true
		}
		return "", 0, true
	}
	if *format == helptext.Text {
		fmt.Println(cat.Source)
	}
	return *format, 0, false
}

func launchAgentCatalog(name string, s agent.Schema, format string) int {
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
		return ModelsVerb([]string{"--live", "--profile", n, "--format", format})
	case len(s.CLI.ModelsArgv) > 0:
		return LaunchAgent(name, s.CLI.ModelsArgv)
	default:
		fmt.Printf("%s: no model catalog\n", name)
		return 0
	}
}

// launchFlags lists the harness flags a bare launch consumes (agent.ParseArgs) for the help page.
func launchFlags(name string) *flag.FlagSet {
	fs := helptext.NewFlagSet(name, helptext.Page{Usage: "claude-code " + name}) // display only; ParseArgs parses
	fs.String("model", "", "select the model for this launch")
	profileUsage := "harness profile for this launch"
	if name == "codex" {
		profileUsage += " (codex's own: -p NAME)"
	}
	helptext.NameVar(fs, new(string), "profile", profileUsage)
	fs.Bool("direct", false, "skip the proxy; hand the CLI the profile route")
	fs.Bool("print-env", false, "print the resolved binary, argv and env; do not launch")
	fs.String("overlay", "", "JSON {set,unset,keep} file over the child's env")
	if name == "codex" {
		fs.String("f", "", "config.toml whose keys ride as -c overrides")
	}
	return fs
}
