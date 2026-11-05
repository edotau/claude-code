package cli

import (
	"context"
	"github.com/edotau/claude-code/internal/vault/vaultcmd"
	"io"
	"os"

	"github.com/edotau/claude-code/internal/claude"
	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

var anthropicPage = helptext.Page{
	Usage: "claude-code anthropic [--oauth[=NAME]] [--x-api-key[=NAME]] [--model M]\n" +
		"[--print-env] [--keep-env NAMES] [--overlay FILE] [claude args]",
	About: "Claude Code on first-party Anthropic. No flag uses your claude.ai login. A bare flag takes no " +
		"value; =NAME picks the vault key.",
	Hint: "claude-code anthropic " + helptext.DocsFlag + " prints claude's own help.",
}

// anthropicHelp renders the page with its flag rows; the flags are parsed by claude.ParseAnthropicArgs.
func anthropicHelp(w io.Writer) {
	fs := helptext.NewFlagSet("anthropic", anthropicPage)
	fs.Bool("oauth", false, "setup-token login; =NAME picks the vault key")
	fs.Bool("x-api-key", false, "API key login; =NAME picks the vault key")
	fs.String("model", "", "alias (opus), version (opus-5-5) or wire id: the main model")
	fs.Bool("print-env", false, "print the resolved launch; never prompts")
	fs.String("keep-env", "", "env names the credential scrub keeps")
	fs.String("overlay", "", "JSON {set,unset,keep} file over the child's env")
	p := anthropicPage
	p.Flags = fs
	p.Fprint(w)
}

// cmdAnthropic is `claude-code anthropic`: first-party Claude Code with a login, OAuth token or API key.
func cmdAnthropic(args []string) int {
	if helptext.WantsHelp(args) {
		anthropicHelp(os.Stdout)
		return 0
	}
	if len(args) == 1 && args[0] == helptext.DocsFlag {
		return helptext.WrappedDocs("claude")
	}
	if err := databricks.RemovedFlagHint(args); err != nil {
		return helptext.Fail("%v", err)
	}
	o, rest, err := claude.ParseAnthropicArgs(args)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	secret := ""
	if o.Mode != claude.AuthLogin {
		if secret, err = claude.AnthropicSecret(o, vaultSet); err != nil {
			return helptext.Fail("%v", err)
		}
	}
	plan, err := claude.Anthropic(context.Background(), o, rest, secret)
	return runPlan("claude", plan, o.LaunchOptions, err)
}

// vaultSet stores a missing secret through the vault verb; claude.AnthropicSecret calls it after the user confirms.
func vaultSet(name string) int { return vaultcmd.Run([]string{"set", name}) }
