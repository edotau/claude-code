package cli

import (
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/cli/ui/helptext"
	"github.com/edotau/claude-code/internal/cli/ui/tui"
	"github.com/edotau/claude-code/internal/platform/vault/vaultdoc"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// vaultEnvSet writes one env value and prints the one-line change.
func vaultEnvSet(name, value string, dryRun, yes bool) int {
	if code, stop := guardEnvChange("set", name, value, dryRun, yes); stop {
		return code
	}
	ch, err := vaultdoc.SetEnv(name, value, dryRun)
	if err != nil {
		return helptext.Fail("vault set env: %v", err)
	}
	return printEnvChange("set", ch, dryRun)
}

// vaultEnvUnset removes one env value and prints the one-line change.
func vaultEnvUnset(name string, dryRun, yes bool) int {
	if code, stop := guardEnvChange("unset", name, "", dryRun, yes); stop {
		return code
	}
	ch, err := vaultdoc.UnsetEnv(name, dryRun)
	if err != nil {
		return helptext.Fail("vault rm env: %v", err)
	}
	return printEnvChange("unset", ch, dryRun)
}

// guardEnvChange asks before a set or unset replaces an existing value; stop ends the verb with code.
func guardEnvChange(verb, name, value string, dryRun, yes bool) (code int, stop bool) {
	if dryRun || yes {
		return 0, false
	}
	var ch vaultdoc.EnvChange
	var err error
	if verb == "set" {
		ch, err = vaultdoc.SetEnv(name, value, true)
	} else {
		ch, err = vaultdoc.UnsetEnv(name, true)
	}
	if err != nil || ch.Old == "" || ch.Old == ch.New {
		return 0, false
	}
	q := fmt.Sprintf("%s is %s; change it to %s?", name, ch.Old, ch.New)
	if verb == "unset" {
		q = fmt.Sprintf("remove %s (%s)?", name, ch.Old)
	}
	if !tui.StdinIsTerminal() {
		fmt.Fprintf(os.Stderr, "vault env: %s already has a value (%s); pass --yes to %s it\n", name, ch.Old,
			map[string]string{"set": "overwrite", "unset": "remove"}[verb])
		return 2, true
	}
	if !tui.ConfirmKey(q, false) {
		fmt.Fprintln(os.Stderr, "vault: nothing changed")
		return 0, true
	}
	return 0, false
}

// printEnvChange words the change by its verb (a dry run on stdout, a write on stderr like vault set).
func printEnvChange(verb string, ch vaultdoc.EnvChange, dryRun bool) int {
	line := fmt.Sprintf("env %s set to %s (was not set)", ch.Name, ch.New)
	switch {
	case verb == "unset":
		line = fmt.Sprintf("env %s removed (was %s)", ch.Name, ch.Old)
	case ch.Old != "":
		line = fmt.Sprintf("env %s changed from %s to %s", ch.Name, ch.Old, ch.New)
	}
	msg, out := line, os.Stderr
	switch {
	case ch.Old == ch.New && ch.Old == "":
		msg = fmt.Sprintf("env %s is not set; nothing written", ch.Name)
	case ch.Old == ch.New:
		msg = fmt.Sprintf("env %s already %s; nothing written", ch.Name, ch.New)
	case dryRun:
		msg += "; nothing written"
	}
	if dryRun {
		msg, out = "dry run: "+msg, os.Stdout
	}
	fmt.Fprintln(out, msg)
	if _, ref := providers.SecretRefName(ch.Name, ch.Old); ref && ch.New == "" {
		fmt.Fprintf(os.Stderr, "the sealed secret %s is kept; `claude-code vault rm secrets %s` removes it\n", ch.Name, ch.Name)
	}
	return 0
}

// orUnset is v, or "(unset)" when empty.
func orUnset(v string) string {
	if v == "" {
		return "(unset)"
	}
	return v
}
