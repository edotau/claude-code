package gateway

import (
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

func cmdGatewayToken(args []string) int {
	fs := helptext.NewFlagSet("gateway token", helptext.Page{
		Usage: "claude-code gateway token --profile p",
		About: "Print a proxy session token for a credential helper (stdout only).\n" +
			"The token is bound to the proxy daemon and scoped to one profile.",
	})
	profile := fs.String("profile", "", "profile the token is scoped to (required)")
	cfg := fs.String("config-dir", "", "CLAUDE_CONFIG_DIR to use (native Windows helper)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() != 0 || *profile == "" {
		return helptext.Fail("gateway token: want --profile <name> and no other arguments")
	}
	token, code := databricks.MintProxyToken(*profile, *cfg)
	if code != 0 {
		return code
	}
	return printToken(token)
}

func printToken(token string) int {
	if _, err := fmt.Fprintln(os.Stdout, token); err != nil {
		return 1
	}
	return 0
}
