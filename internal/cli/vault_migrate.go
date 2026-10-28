package cli

import (
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/vaultmigrate"
)

// cmdVaultMigrate is `vault migrate [--apply] [--format json]`: the one-time rename to the naming standard.
func cmdVaultMigrate(args []string) int {
	fs := helptext.NewFlagSet("vault migrate", helptext.Page{
		Usage: "claude-code vault migrate [--apply] [--format text|json]",
		About: "Rename pins, secrets and overlays to the naming standard. A dry run unless\n" +
			"--apply, which first backs up state/vault.db and changes all or nothing.",
	})
	apply := fs.Bool("apply", false, "write the changes (default: print the plan only)")
	format := outfmt.Flag(fs)
	pos, err := outfmt.Parse(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) > 0 {
		return vaultUsageErr("migrate", pos)
	}
	if err := outfmt.Check(*format); err != nil {
		return fail("vault migrate: %v", err)
	}
	reg, err := vaultmigrate.Current()
	if err != nil {
		return fail("%v", err)
	}
	plan, err := vaultmigrate.Compute(reg)
	if err != nil {
		return fail("vault migrate: %v", err)
	}
	backup := ""
	if *apply {
		res, err := vaultmigrate.Apply(reg)
		if err != nil {
			return fail("%v", err)
		}
		plan, backup = res.Plan, res.Backup
	}
	if *format == outfmt.JSON {
		return writeMigrateJSON(plan, *apply, backup)
	}
	printMigrate(plan, *apply, backup)
	return 0
}

func writeMigrateJSON(plan *vaultmigrate.Plan, applied bool, backup string) int {
	err := outfmt.Write(os.Stdout, struct {
		Applied  bool               `json:"applied"`
		Backup   string             `json:"backup,omitempty"`
		NeedsKey bool               `json:"needs_key"`
		Plan     *vaultmigrate.Plan `json:"plan"`
	}{applied, backup, plan.NeedsKey(), plan})
	if err != nil {
		return fail("vault migrate: %v", err)
	}
	return 0
}

func printMigrate(plan *vaultmigrate.Plan, applied bool, backup string) {
	if plan.Empty() {
		fmt.Println("vault is already on the naming standard; nothing to migrate")
		return
	}
	plan.Render(os.Stdout)
	switch {
	case applied && backup != "":
		fmt.Printf("\nmigrated %d changes; backup %s\n", len(plan.Changes), backup)
	case applied:
		fmt.Printf("\nmigrated %d changes\n", len(plan.Changes))
	default:
		fmt.Printf("\n%d changes; dry run, rerun with --apply to write them\n", len(plan.Changes))
	}
	if plan.NeedsKey() && !applied {
		fmt.Println("applying re-seals secrets, so it needs the master key (OS keyring, HARNESS_VAULT_KEY or key_command)")
	}
}
