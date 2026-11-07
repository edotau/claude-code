// Package providerscmd holds the models, switch and resolve verbs.
package providerscmd

import (
	"context"
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
)

// printLines prints each line of a rendered view.
func printLines(lines []string) {
	for _, l := range lines {
		fmt.Println(l)
	}
}

var modelsPage = helptext.Page{
	Usage: "claude-code models [flags]",
	About: "Shows which model each alias (opus, sonnet, haiku, fable) resolves to on\n" +
		"the session profile. --pin and --unpin write the vault pins; --live lists the\n" +
		"profile's catalog.",
	Hint: "A CLAUDE_<ALIAS>_MODEL vault pin beats the profile's own model.",
}

// Models is the models verb: alias resolution, pins, the live catalog, and the discover subcommand.
func Models(args []string) int {
	if len(args) > 0 && args[0] == "discover" {
		return modelsDiscover(args[1:])
	}
	fs := helptext.NewFlagSet("models", modelsPage)
	provider := fs.String("profile", "", "profile to resolve (default: pin, then registry default)")
	unpin := fs.Bool("unpin", false, "clear every model pin")
	live := fs.Bool("live", false, "list the models the profile serves (GET /models)")
	dryRun := fs.Bool("dry-run", false, "with --pin/--unpin: print the agent record change, write nothing")
	format := helptext.FormatFlag(fs)
	var pins []string
	fs.Func("pin", "alias=model, or all=model (repeatable)", func(v string) error {
		pins = append(pins, v)
		return nil
	})
	args, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "claude-code: models: unexpected argument %q\n", args[0])
		return 2
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("models", err)
	}
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("%v", err)
	}
	if code, handled := applyModelPins(reg, *provider, *unpin, *dryRun, pins); handled {
		return code
	}
	sel, err := providers.Select(reg, *provider)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	return renderModelSelection(reg, sel, *format, *live)
}

func applyModelPins(reg *providers.Registry, provider string, unpin, dryRun bool, pins []string) (code int, handled bool) {
	if !unpin && len(pins) == 0 {
		return 0, false
	}
	set, err := agentconfig.ParseModelPins(reg, provider, unpin, pins)
	if err != nil {
		return helptext.Fail("%v", err), true
	}
	warn, err := copilot.ValidateCopilotPins(provider, pins)
	if err != nil {
		return helptext.Fail("%v", err), true
	}
	if warn != "" {
		fmt.Fprintln(os.Stderr, "claude-code: models:", warn)
	}
	if dryRun {
		return printRecordDiff(func(m map[string]vault.AgentRecord) { m["claude"] = agentconfig.WithClaudeModels(m["claude"], set) }, nil), true
	}
	if err := agentconfig.ApplyClaudeModels(set); err != nil {
		return helptext.Fail("%v", err), true
	}
	return 0, false
}

func renderModelSelection(reg *providers.Registry, sel providers.Selection, format string, live bool) int {
	if live {
		return listLive(reg, sel.Provider, format)
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, map[string]any{"profile": sel.Provider.Name, "proxy": sel.NeedsRouter(reg), "models": sel.SpecsByAlias()}); err != nil {
			return helptext.Fail("models: %v", err)
		}
		return 0
	}
	printLines(providers.SelectionLines(reg, sel))
	return 0
}

// listLive prints the provider's live model catalog (github-copilot's own SDK, every other provider's /models GET).
func listLive(reg *providers.Registry, p *providers.Provider, format string) int {
	var own func(context.Context) ([]providers.ModelInfo, error)
	if p.Provider == "github" {
		own = copilot.ListModels
	}
	cat, err := providers.LoadCatalog(reg, p, own)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	if cat.Note != "" {
		fmt.Fprintln(os.Stderr, "claude-code: models:", cat.Note)
	}
	if cat.GatewayErr != nil {
		fmt.Fprintln(os.Stderr, "claude-code: models: ai gateway listing unavailable:", cat.GatewayErr)
	}
	if format == helptext.JSON {
		return helptext.WriteJSONOrFail("models", cat.Rows())
	}
	printLines(cat.TableLines())
	return 0
}
