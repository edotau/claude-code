package databricks

import (
	"context"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// MintProxyToken ensures the proxy is up and mints a token for profile; a nonzero code is the exit status.
func MintProxyToken(profile, cfgDir string) (string, int) {
	if cfgDir != "" {
		if err := os.Setenv("CLAUDE_CONFIG_DIR", cfgDir); err != nil {
			return "", helptext.Fail("gateway token: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	token, err := gateway.Token(ctx, profile)
	if err != nil {
		return "", helptext.Fail("gateway token: %v", err)
	}
	return token, 0
}
