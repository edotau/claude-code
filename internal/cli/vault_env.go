package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/tui"
	"github.com/edotau/claude-code/internal/vault/vaultdoc"
)

var vaultEnvPage = helptext.Page{
	Usage: "claude-code vault env <set|unset> NAME [VALUE] [flags]",
	About: "Set or remove one value in the vault's env bucket (injected into every launch;\n" +
		"listed by vault ls env). The bucket is not sealed: secrets go through vault set.",
	Commands: [][2]string{
		{"set", "NAME VALUE: write the value, as configure → environment does"},
		{"unset", "NAME: remove the value"},
	},
}

// envOperands is each vault env subcommand's operands, as its usage line shows them.
var envOperands = map[string]string{"set": "NAME VALUE", "unset": "NAME"}

// cmdVaultEnv is `vault env set NAME VALUE` and `vault env unset NAME`.
func cmdVaultEnv(args []string) int {
	if code, ok := helpOrBare(vaultEnvPage, args); ok {
		return code
	}
	sub, args := args[0], args[1:]
	operands, ok := envOperands[sub]
	if !ok {
		return unknownSub("vault env", sub, vaultEnvPage)
	}
	usage := "claude-code vault env " + sub + " " + operands + " [flags]"
	fs := helptext.NewFlagSet("vault env "+sub, helptext.Page{Usage: usage})
	dryRun := fs.Bool("dry-run", false, "report what would change; write nothing")
	yes := fs.Bool("yes", false, "overwrite or remove an existing value without asking; required off a terminal")
	fs.BoolVar(yes, "y", false, "same as --yes")
	words, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(words) != len(strings.Fields(operands)) {
		fs.Usage()
		return 2
	}
	if sub == "unset" {
		return vaultEnvUnset(words[0], *dryRun, *yes)
	}
	return vaultEnvSet(words[0], words[1], *dryRun, *yes)
}

// vaultEnvSet writes one env value and prints the one-line change.
func vaultEnvSet(name, value string, dryRun, yes bool) int {
	if code, stop := guardEnvChange("set", name, value, dryRun, yes); stop {
		return code
	}
	ch, err := vaultdoc.SetEnv(name, value, dryRun)
	if err != nil {
		return helptext.Fail("vault env set: %v", err)
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
		return helptext.Fail("vault env unset: %v", err)
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
		fmt.Fprintf(os.Stderr, "vault env %s: %s already has a value (%s); pass --yes to %s it\n", verb, name, ch.Old,
			map[string]string{"set": "overwrite", "unset": "remove"}[verb])
		return 2, true
	}
	if !tui.ConfirmKey(q, false) {
		fmt.Fprintln(os.Stderr, "vault env: nothing changed")
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
		fmt.Fprintf(os.Stderr, "the sealed secret %s is kept; `claude-code vault delete %s` removes it\n", ch.Name, ch.Name)
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
