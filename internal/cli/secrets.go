package cli

import (
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/secrets"
)

// extractBool pulls a `--name` boolean flag out of args wherever it appears (NAME --flag and
// --flag NAME must both work — the stdlib `flag` package only accepts flags before positionals).
func extractBool(args []string, name string) (bool, []string) {
	flag := "--" + name
	found := false
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if a == flag {
			found = true
			continue
		}
		rest = append(rest, a)
	}
	return found, rest
}

// secretAudits runs the requested credential audits for doctor --scan/--git/--secrets; worst exit wins.
func secretAudits(scan, git, vaultChecks bool, targets []string, rev string, since int) int {
	rc, sep := secrets.ExitClean, ""
	if scan {
		rc = max(rc, (&secrets.Scanner{Out: os.Stdout}).ScanConfigs(targets))
		sep = "\n"
	}
	if git {
		fmt.Print(sep)
		dir, err := os.Getwd()
		if err != nil {
			return fail("doctor --git: %v", err)
		}
		rc, sep = max(rc, secrets.ScanGit(os.Stdout, dir, rev, since)), "\n"
	}
	if vaultChecks {
		fmt.Print(sep)
		if !launch.VaultDoctor(os.Stdout) {
			rc = max(rc, secrets.ExitFindings)
		}
	}
	return rc
}
