package databricks

import (
	"fmt"
	"os"
	"slices"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
)

var identityPage = helptext.Page{
	Usage: "claude-code databricks identity --profile <name> auto|user|sp [flags]",
	About: "Pin a profile's identity mode, as configure databricks → Identity does; the paired " +
		"service principal and its secret are kept. sp needs a paired service principal.",
}

// identityModes are the three identity values a profile row stores.
var identityModes = []string{providers.IdentityAuto, providers.IdentityUser, providers.IdentitySP}

// IdentityChange is one profile identity write: the mode before and after, and the paired service principal id.
type IdentityChange struct{ Profile, Old, New, SP string }

// SetIdentity pins profile's identity mode through SaveIdentity, the configure menu's writer; dryRun writes nothing.
func SetIdentity(reg *providers.Registry, profile, mode string, dryRun bool) (IdentityChange, error) {
	if !slices.Contains(providers.DatabricksProfiles(reg), profile) {
		return IdentityChange{}, fmt.Errorf("%q is not a Databricks profile", profile)
	}
	if !slices.Contains(identityModes, mode) {
		return IdentityChange{}, fmt.Errorf("identity %q must be auto, user or sp", mode)
	}
	p := reg.Providers[profile]
	old, _ := providers.ResolveIdentity(p.Identity, "")
	ch := IdentityChange{Profile: profile, Old: old, New: mode, SP: p.Auth.SPClientID}
	if mode == providers.IdentitySP {
		if ch.SP == "" { // SaveIdentity refuses an sp row with no client id, with the menu's message, and writes nothing
			return ch, fmt.Errorf("%w; pair one in `claude-code configure databricks` → Identity",
				SaveIdentity(reg, profile, mode, "", ""))
		}
		name := providers.SecretName(providers.GroupDatabricks, profile, providers.AuthM2M)
		if _, ok, err := vault.ReadSecret(name); err != nil || !ok {
			return ch, fmt.Errorf("%s: SP %s has no secret; seal one with `claude-code vault set %s`, or keep identity %s",
				profile, ch.SP, name, old)
		}
	}
	if dryRun || p.Identity == mode {
		return ch, nil
	}
	return ch, SaveIdentity(reg, profile, mode, ch.SP, "")
}

// identity is `databricks identity --profile <p> auto|user|sp`: the scriptable form of the menu's Identity page.
func identity(args []string) int {
	fs := helptext.NewFlagSet("databricks identity", identityPage)
	var profile string
	profileFlagVar(fs, &profile, "Databricks profile to change (required)")
	dryRun := fs.Bool("dry-run", false, "report the change; write nothing")
	rest, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if profile == "" || len(rest) != 1 || !slices.Contains(identityModes, rest[0]) {
		fs.Usage()
		return 2
	}
	reg, err := providers.Load()
	if err != nil {
		return fail("identity: load providers: %v", err)
	}
	ch, err := SetIdentity(reg, profile, rest[0], *dryRun)
	if err != nil {
		return fail("identity: %v", err)
	}
	line := fmt.Sprintf("%s: identity %s → %s", ch.Profile, ch.Old, ch.New)
	if ch.New == providers.IdentitySP {
		line += " · SP " + ch.SP
	}
	switch {
	case *dryRun:
		line = "would set " + line + "; nothing written"
	case ch.Old == ch.New && reg.Providers[profile].Identity == ch.New:
		line = fmt.Sprintf("%s: identity already %s; nothing written", ch.Profile, ch.New)
	}
	fmt.Fprintln(os.Stdout, line)
	return 0
}
