package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/proxy"
)

var (
	tokenEnsure = proxy.Ensure
	tokenState  = proxy.Running
	tokenMint   = proxy.MintSessionToken
)

func cmdGatewayToken(args []string) int {
	fs := helptext.NewFlagSet("gateway token", helptext.Page{
		Usage: "claude-code gateway token --profile p",
		About: "Print a proxy session token for a credential helper (stdout only).\n" +
			"The token is bound to the proxy daemon and scoped to one profile.",
	})
	profile := fs.String("profile", "", "profile the token is scoped to (required)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() != 0 || *profile == "" {
		return fail("gateway token: want --profile <name> and no other arguments")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := tokenEnsure(ctx); err != nil {
		return fail("gateway token: proxy unavailable; run gateway proxy --start")
	}
	st, ok := tokenState(ctx)
	if !ok {
		return fail("gateway token: proxy not running")
	}
	token, err := tokenMint(ctx, *profile, st.PID)
	if err != nil {
		return fail("gateway token: %v", err)
	}
	if _, err := fmt.Fprintln(os.Stdout, token); err != nil {
		return 1
	}
	return 0
}
