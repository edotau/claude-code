package vaultcmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/tui"
)

// vaultList prints the selected sections (default all): secrets masked, then the vault's non-secret config.
func vaultList(args []string) int {
	fs := helptext.NewFlagSet("vault list", helptext.Page{
		Usage: "claude-code vault list [<section>...|all]\n[--format text|json] [--tailscale]",
		About: "Print what the vault stores, secrets masked. Sections: " +
			strings.Join(Sections, ", ") + "; bare or all prints every one.",
	})
	ts := fs.Bool("tailscale", false, "show Tailscale credential presence, not JSON")
	format := fs.String("format", "", "text or json (default: text on a terminal, json otherwise)")
	names, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if *ts {
		if len(names) > 0 {
			return vaultUsageErr("list", names)
		}
		Tailscale(os.Stdout)
		return 0
	}
	sel, err := Select(names)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vault list:", err)
		return 2
	}
	f := *format
	if f == "" {
		f = helptext.JSON
		if tui.StdoutIsTerminal() {
			f = helptext.Text
		}
	}
	if err := helptext.CheckFormat(f); err != nil {
		helptext.Fail("vault list: %v", err)
		return 2
	}
	if err := List(os.Stdout, f, sel); err != nil {
		return helptext.Fail("vault list: %v", err)
	}
	return 0
}
