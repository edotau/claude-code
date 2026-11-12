package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/edotau/claude-code/internal/claude"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// anthropicSpec is the completion node for the anthropic verb.
func anthropicSpec() *helptext.Node {
	flag, boolFlag := helptext.Plain, helptext.BoolFlag
	return &helptext.Node{Sub: map[string]*helptext.Node{"science": {}},
		Flags: []helptext.Flag{boolFlag("oauth"), boolFlag("x-api-key"), flag("model"), flag("profile"),
			boolFlag("print-env"), flag("keep-env"), flag("overlay")}}
}

// cmdClaude is `claude-code claude`: Claude Code on the selected profile.
func cmdClaude(args []string) int {
	code := claude.Launch("", args)
	if code == 0 && len(args) == 1 && helptext.WantsHelp(args) { // claude's own help is relayed; say what the harness takes first
		fmt.Println("\nclaude-code claude takes --profile P --model M --as user|sp --overlay F\n" +
			"--keep-env NAMES --print-env; everything else goes to claude.")
	}
	return code
}

var anthropicPage = helptext.Page{
	Usage: "claude-code anthropic [--oauth[=NAME]] [--x-api-key[=NAME]] [--model M]\n" +
		"[--profile P] [--print-env] [--keep-env NAMES] [--overlay FILE] [args]\n" +
		"claude-code anthropic science [--print-env] [--port N] [args]",
	About: "Claude Code on Anthropic directly. No flag uses your claude.ai login. A bare flag takes no " +
		"value; =NAME picks the vault key. --profile runs on that registry profile instead.",
	Commands: [][2]string{{"science", "Claude for Life Sciences, own login; --print-env, --port N"}},
	Hint:     "claude-code anthropic " + helptext.DocsFlag + " prints claude's own help.",
}

// anthropicHelp renders the page with its flag rows; the flags are parsed by claude.LaunchAnthropic.
func anthropicHelp(w io.Writer) {
	fs := helptext.NewFlagSet("anthropic", anthropicPage)
	fs.Bool("oauth", false, "setup-token login; =NAME picks the vault key")
	fs.Bool("x-api-key", false, "API key login; =NAME picks the vault key")
	fs.String("model", "", "opus|sonnet|haiku|fable, a version (opus-5-5) or wire id")
	fs.String("profile", "", "run on this registry profile instead")
	fs.Bool("print-env", false, "print the resolved launch; never prompts")
	fs.String("keep-env", "", "env names the credential scrub keeps")
	fs.String("overlay", "", "env layer {set,unset,keep}: file, inline JSON, or -")
	p := anthropicPage
	p.Flags = fs
	p.Fprint(w)
}

// cmdAnthropic is `claude-code anthropic`: Claude Code on Anthropic with a login, OAuth token or API key.
func cmdAnthropic(args []string) int {
	if len(args) > 0 && args[0] == "science" {
		return claude.LaunchScience(args[1:])
	}
	if len(args) > 0 && helptext.WantsHelp(args[:1]) { // only a leading help flag; `anthropic mcp --help` goes to claude
		anthropicHelp(os.Stdout)
		return 0
	}
	if len(args) == 1 && args[0] == helptext.DocsFlag {
		return helptext.WrappedDocs("claude")
	}
	return claude.LaunchAnthropic(args, func(name string) int { return cmdVault([]string{"set", name}) })
}
