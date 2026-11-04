package menu

import (
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/configure"
	uidialog "github.com/edotau/claude-code/internal/ui/dialog"
	"github.com/edotau/claude-code/internal/vault"
)

// replaceKey pastes a new value for kt's keyed credential, probes it live before it overwrites the
// stored one (a rejected/unreachable paste asks whether to keep the old one instead), and writes it
// back to the vault.
func replaceKey(d *dialog, kt configure.KeyTarget) bool {
	if kt.EnvVar == "" {
		return false
	}
	if _, src, _ := vault.Get(kt.EnvVar); src == "env" {
		d.printf("%s: env-only, not in the vault; edit where exported\n", kt.EnvVar)
		return false
	}
	if !actionConfirmed("Paste a new " + kt.EnvVar + " to rotate it") {
		return false
	}
	if u := configure.ConsoleURLs[kt.EnvVar]; u != "" && d.shell().Confirm("Open "+u+" in your browser to create the new key?") {
		openBrowser(u)
	}
	val, err := secretLine(d, "Paste the new value for "+kt.EnvVar+" (input hidden, Enter when done): ")
	if err != nil || strings.TrimSpace(val) == "" {
		d.printf("%s: no value entered\n", kt.EnvVar)
		return false
	}
	_, v := configure.ParseAssignment(val)
	if kt.Provider != nil {
		status, detail := configure.ProbeValue(kt.Provider, kt.EnvVar, v)
		if status == "rejected" || status == "unreachable" {
			d.printf("%s: new value %s (%s)\n", kt.EnvVar, status, detail)
			if d.shell().Confirm("Keep the old key and discard this paste?") {
				return false
			}
		}
	}
	return storeValue(d, kt.EnvVar, v)
}

// runMint asks first (a stray Enter must never start a mint), then runs the token-minting CLI.
func runMint(argv []string) (string, error) {
	if !actionConfirmed("Run `" + strings.Join(argv, " ") + "` to mint a new token") {
		return "", nil
	}
	return configure.RunMint(argv)
}

// regenerate re-runs an agent's own login or mint CLI to rotate a credential that is already present.
// A login reports the outcome (the session lives in the agent's own file, not the vault).
// A mint writes the printed token back to the vault.
func regenerate(d *dialog, kt configure.KeyTarget, argv []string, mint bool) bool {
	if !mint {
		if err := configure.RunLogin(argv); err != nil {
			d.printf("%s: %v\n", kt.Label, err)
			return false
		}
		d.printf("%-10s logged in again\n", kt.Label)
		return true
	}
	tok, err := runMint(argv)
	if err != nil {
		d.printf("%s: %v\n", kt.Label, err)
		return false
	}
	if tok == "" {
		d.printf("%s: no token found in the output; try again or paste one\n", kt.EnvVar)
		return false
	}
	if _, src, _ := vault.Get(kt.EnvVar); src == "env" {
		d.printf("%s: minted a new token, saving it to the vault; the exported %s still overrides it\n", kt.Label, kt.EnvVar)
	}
	return storeValue(d, kt.EnvVar, tok)
}

// runRotate offers kt's rotate options (generate first, else paste) and runs the chosen one; false
// means skipped, cancelled, or nothing to rotate.
func runRotate(d *dialog, kt configure.KeyTarget) bool {
	opts := configure.RotateOptions(kt)
	if len(opts) == 0 {
		return false
	}
	chosen := 0
	if len(opts) > 1 {
		if isStdinTTY() && d.o.In == os.Stdin {
			rows := make([]uidialog.Row, len(opts))
			for i, o := range opts {
				rows[i] = uidialog.Row{Name: o.Action, Detail: o.Desc}
			}
			idx, err := d.shell().Pick(kt.Label+" — rotate which credential?", uidialog.Pairs(rows))
			if err != nil || idx < 0 {
				return false
			}
			chosen = idx
		} else if !d.shell().Confirm(opts[0].Label() + "?") {
			chosen = 1
		}
	}
	o := opts[chosen]
	switch {
	case len(o.Argv) > 0:
		return regenerate(d, kt, o.Argv, false)
	case len(o.Mint) > 0:
		return regenerate(d, kt, o.Mint, true)
	default:
		return replaceKey(d, kt)
	}
}

// ensureKey makes sure kt's keyed credential is usable before a sub-picker pins it: absent asks to add
// one, present defaults to keeping it and only replaces on an explicit no.
func ensureKey(d *dialog, kt configure.KeyTarget, present bool) bool {
	if !present {
		return resolveKey(d, kt)
	}
	if d.shell().Confirm(kt.Label + " key present — keep it?") {
		return true
	}
	return replaceKey(d, kt)
}
