package cli

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/config"
	"github.com/edotau/claude-code/internal/helptext"
)

const configUsage = `usage: claude-code config <subcommand>
show        print the vault's pins, env, overlays and key_command as JSON;
            secrets never appear (use claude-code secrets)
edit        $VISUAL/$EDITOR on that JSON; saved in one transaction,
            refused if the vault changed meanwhile
migrate     import env.d into the vault; sources become *.bak [--dry-run]
finalize    delete the *.bak files and imported keyring copies once
            verified [--discard NAME]...
rollback    restore the *.bak files and set the vault aside
rotate-key  re-seal every secret under a new master key; the old key
            stays in the keyring`

// configVerbs is the config subcommand table.
var configVerbs = map[string]func(args []string) int{
	"show": configShow, "edit": configEdit, "migrate": configMigrate,
	"finalize": configFinalize, "rollback": configRollback, "rotate-key": configRotateKey,
}

// cmdConfig reads and edits the vault's non-secret config through one JSON document.
func cmdConfig(args []string) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "", "-h", "--help":
		helptext.Fprint(os.Stdout, configUsage+"\n")
		return 0
	}
	if run, ok := configVerbs[sub]; ok {
		return run(args)
	}
	fmt.Fprintf(os.Stderr, "config: unknown subcommand %q\n", sub)
	helptext.Fprint(os.Stderr, configUsage+"\n")
	return 2
}

// configUsageErr reports a stray argument to a config subcommand.
func configUsageErr(sub string, args []string) int {
	fmt.Fprintf(os.Stderr, "config %s: unexpected argument %q\n", sub, args[0])
	helptext.Fprint(os.Stderr, configUsage+"\n")
	return 2
}

func configShow(args []string) int {
	if len(args) > 0 {
		return configUsageErr("show", args)
	}
	out, err := config.Export()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config show:", err)
		return 1
	}
	if _, err := os.Stdout.Write(out); err != nil {
		fmt.Fprintln(os.Stderr, "config show:", err)
		return 1
	}
	return 0
}

func configEdit(args []string) int {
	if len(args) > 0 {
		return configUsageErr("edit", args)
	}
	if err := config.Edit(os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "config edit:", err)
		return 1
	}
	return 0
}

func configMigrate(args []string) int {
	fs := flag.NewFlagSet("config migrate", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "parse and report; change nothing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		return configUsageErr("migrate", fs.Args())
	}
	return configRun("migrate", config.Migrate(os.Stdout, *dry))
}

// discardFlag collects repeatable --discard NAME acknowledgements.
type discardFlag []string

func (d *discardFlag) String() string     { return strings.Join(*d, ",") }
func (d *discardFlag) Set(s string) error { *d = append(*d, s); return nil }

func configFinalize(args []string) int {
	fs := flag.NewFlagSet("config finalize", flag.ContinueOnError)
	var discard discardFlag
	fs.Var(&discard, "discard", "drop NAME's differing keyring/backup value (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		return configUsageErr("finalize", fs.Args())
	}
	return configRun("finalize", config.Finalize(os.Stdout, discard))
}

func configRollback(args []string) int {
	if len(args) > 0 {
		return configUsageErr("rollback", args)
	}
	return configRun("rollback", config.Rollback(os.Stdout))
}

func configRotateKey(args []string) int {
	if len(args) > 0 {
		return configUsageErr("rotate-key", args)
	}
	return configRun("rotate-key", config.RotateKey(os.Stdout))
}

// configRun maps a subcommand's error to its exit code.
func configRun(sub string, err error) int {
	if err != nil {
		fmt.Fprintf(os.Stderr, "config %s: %v\n", sub, err)
		return 1
	}
	return 0
}
