package cli

import (
	"fmt"

	"github.com/edotau/claude-code/internal/profile"
	"github.com/edotau/claude-code/internal/providers"
)

// cmdVendorEnv is `<vendor> env [env]`: print the vendor's account, or repoint only its profile row.
func cmdVendorEnv(vendor string, args []string) int {
	reg, err := providers.Load()
	if err != nil {
		return fail("%s env: %v", vendor, err)
	}
	if len(args) == 0 {
		if row, _ := reg.AgentRow(vendor); row.Account != "" {
			fmt.Printf("%s: %s\n", vendor, row.Account)
			return 0
		}
		fmt.Printf("%s: no account in the active profile (launches use today's selection)\n", vendor)
		return 0
	}
	if len(args) > 1 {
		return fail("%s env: want one of dev, qa, prod", vendor)
	}
	if _, _, err := profile.UseAgentAccount(true, reg, vendor, args[0]); err != nil {
		return fail("%s env: %v", vendor, err)
	}
	fmt.Printf("%s: now on %s; relaunch `claude-code %s` to apply\n", vendor, args[0], vendor)
	if vendor == "codex" {
		fmt.Println("plain codex: run `claude-code configure --reset --vendors` to re-render config.toml")
	}
	return 0
}
