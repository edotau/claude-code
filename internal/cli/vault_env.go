package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
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
	words, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(words) != len(strings.Fields(operands)) {
		fs.Usage()
		return 2
	}
	if sub == "unset" {
		return vaultEnvUnset(words[0], *dryRun)
	}
	return vaultEnvSet(words[0], words[1], *dryRun)
}

// vaultEnvSet writes one env value and prints the one-line change.
func vaultEnvSet(name, value string, dryRun bool) int {
	ch, err := vaultdoc.SetEnv(name, value, dryRun)
	if err != nil {
		return helptext.Fail("vault env set: %v", err)
	}
	return printEnvChange("set", ch, dryRun)
}

// vaultEnvUnset removes one env value and prints the one-line change.
func vaultEnvUnset(name string, dryRun bool) int {
	ch, err := vaultdoc.UnsetEnv(name, dryRun)
	if err != nil {
		return helptext.Fail("vault env unset: %v", err)
	}
	return printEnvChange("unset", ch, dryRun)
}

// printEnvChange prints "env NAME: old → new" (a dry run on stdout, a write on stderr like vault set).
func printEnvChange(verb string, ch vaultdoc.EnvChange, dryRun bool) int {
	old, now := orUnset(ch.Old), orUnset(ch.New)
	line := fmt.Sprintf("env %s: %s → %s", ch.Name, old, now)
	switch {
	case dryRun:
		fmt.Printf("would %s %s; nothing written\n", verb, line)
	case ch.Old == ch.New && ch.Old == "":
		fmt.Fprintf(os.Stderr, "env %s is not set; nothing written\n", ch.Name)
	case ch.Old == ch.New:
		fmt.Fprintf(os.Stderr, "env %s already %s; nothing written\n", ch.Name, now)
	default:
		fmt.Fprintln(os.Stderr, line)
	}
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
