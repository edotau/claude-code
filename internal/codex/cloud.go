package codex

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// cloudVerb is one `codex cloud` subcommand: its positionals (exec joins its words as the query) and its flags.
type cloudVerb struct {
	pos   int
	flags []string
}

var cloudVerbs = map[string]cloudVerb{
	"exec":   {-1, []string{"env", "attempts", "branch"}},
	"status": {1, nil},
	"list":   {0, []string{"env", "limit"}},
	"diff":   {1, []string{"attempt"}},
	"apply":  {1, []string{"attempt"}},
}

var cloudPage = helptext.Page{
	Usage: "claude-code codex cloud <exec|status|list|diff|apply> [flags]",
	About: "Codex Cloud tasks on your ChatGPT login. exec sends the repo to OpenAI's cloud: it needs overlay " +
		`{"cloud":true}; the others only read back.`,
	Commands: [][2]string{
		{"exec", `"<query>" --env ID [--attempts N] [--branch B]`},
		{"status", "<task>"},
		{"list", "[--env ID] [--limit N]"},
		{"diff", "<task> [--attempt N]"},
		{"apply", "<task> [--attempt N]: patch the working tree"},
	},
}

// cloudFlags is verb's flag set: only its own flags plus --overlay, so -h and completion list the right ones.
func cloudFlags(verb string) (fs *flag.FlagSet, overlay *string) {
	fs = helptext.NewFlagSet("codex cloud "+verb, cloudPage)
	for _, n := range cloudVerbs[verb].flags {
		switch n {
		case "env":
			fs.String(n, "", "cloud environment id (exec: required)")
		case "branch":
			fs.String(n, "", "git branch to run (default: current)")
		case "attempts":
			fs.Int(n, 0, "best-of-N attempts")
		case "limit":
			fs.Int(n, 0, "most tasks to show (1-20)")
		case "attempt":
			fs.Int(n, 0, "attempt number (1-based)")
		}
	}
	return fs, fs.String("overlay", "", "config: file, JSON or -; else $"+agentconfig.OverlayVar)
}

// cloudArgv is the `codex cloud` argument list for verb args; the verb's flag set holds only its own flags.
func cloudArgv(verb string, fs *flag.FlagSet, pos []string) ([]string, error) {
	v := cloudVerbs[verb]
	argv := []string{"cloud", verb}
	switch {
	case v.pos < 0 && len(pos) == 0, v.pos >= 0 && len(pos) != v.pos:
		return nil, fmt.Errorf("cloud %s: wrong number of arguments", verb)
	case v.pos < 0:
		pos = []string{strings.Join(pos, " ")}
	}
	var err error
	fs.Visit(func(f *flag.Flag) {
		switch {
		case f.Name == "overlay":
		case strings.HasPrefix(f.Value.String(), "-"):
			err = errors.Join(err, fmt.Errorf("cloud %s: --%s value must not start with -", verb, f.Name))
		default:
			argv = append(argv, "--"+f.Name, f.Value.String())
		}
	})
	if verb == "exec" && fs.Lookup("env").Value.String() == "" {
		err = errors.Join(err, errors.New("cloud exec: --env ID is required (find one with `codex cloud` in a terminal)"))
	}
	if len(pos) > 0 {
		argv = append(argv, "--") // a query or id that starts with - is not a flag
	}
	return append(argv, pos...), err
}

// Cloud is `codex cloud <verb>`: exec needs overlay cloud (it sends the repo), the read-back verbs do not.
func Cloud(args []string) int {
	if len(args) == 0 || helptext.WantsHelp(args[:1]) {
		cloudPage.Fprint(os.Stderr)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	verb := args[0]
	if _, ok := cloudVerbs[verb]; !ok {
		return helptext.FailUsage("codex cloud: unknown subcommand %q (exec|status|list|diff|apply)", verb)
	}
	fs, overlay := cloudFlags(verb)
	pos, err := helptext.ParseArgs(fs, args[1:])
	if err != nil {
		return helptext.ExitCode(err)
	}
	ctx := context.Background()
	argv, bin, err := cloudPlan(ctx, verb, *overlay, fs, pos)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	return agent.RunOwnLogin(ctx, "codex", bin, []string{"OPENAI_BASE_URL"}, argv)
}

// cloudPlan gates the verb on the overlay, then builds its argv and finds the binary.
func cloudPlan(ctx context.Context, verb, overlay string, fs *flag.FlagSet, pos []string) (argv []string, bin string, err error) {
	ov, err := agentconfig.LoadOverlayContext(ctx, overlay, os.Stdin)
	if err != nil {
		return nil, "", err
	}
	if verb == "exec" {
		err = agentconfig.CloudGate(ctx, ov, "codex", "codex cloud exec", "the repo")
	} else {
		err = agentconfig.OwnLogin(ov, "codex", "codex cloud "+verb)
	}
	if err != nil {
		return nil, "", err
	}
	if argv, err = cloudArgv(verb, fs, pos); err != nil {
		return nil, "", err
	}
	bin, err = paths.AgentBin("codex")
	return argv, bin, err
}
