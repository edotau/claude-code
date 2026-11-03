// rotate.go — rotate or replace a credential that is already present: generate a fresh one via the
// agent's own CLI where one exists (`runLogin`/`runMint`), otherwise paste, always probing a keyed
// replacement live before it overwrites the stored value in the vault.
package configure

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

// ProbeValue live-checks a candidate value for p without storing it first, via the env-var seam
// providers.Credential already checks ahead of the vault; the previous env is restored after.
func ProbeValue(p *providers.Provider, envVar, candidate string) (status, detail string) {
	prev, had := os.LookupEnv(envVar)
	if err := os.Setenv(envVar, candidate); err != nil {
		return "unreachable", fmt.Sprintf("cannot set %s for credential probe: %v", envVar, err)
	}
	defer func() {
		if had {
			_ = os.Setenv(envVar, prev)
		} else {
			_ = os.Unsetenv(envVar)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	return providers.ProbeCredential(ctx, p)
}

// RotateOptions lists how an already-present kt can be rotated: its own CLI first (login, then mint —
// the anthropic subscription row carries both, named explicitly), else a plain paste when it is a
// keyed credential with no generator.
func RotateOptions(kt KeyTarget) []AuthOption {
	var opts []AuthOption
	if len(kt.LoginArgv) > 0 {
		opts = append(opts, AuthOption{Action: "Login", Desc: strings.Join(kt.LoginArgv, " ") + " · browser sign-in, refreshes the session", Argv: kt.LoginArgv})
	}
	if len(kt.MintArgv) > 0 {
		opts = append(opts, AuthOption{Action: "OAuth token", Desc: strings.Join(kt.MintArgv, " ") + " · mints a new token, rotates " + kt.EnvVar,
			Mint: kt.MintArgv, Env: kt.EnvVar})
	}
	if len(opts) == 0 && kt.EnvVar != "" && kt.Provider != nil {
		opts = append(opts, AuthOption{Action: "API key", Desc: "paste a new " + kt.EnvVar + " · rotates the stored key", Env: kt.EnvVar})
	}
	return opts
}
