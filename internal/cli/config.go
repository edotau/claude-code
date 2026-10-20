package cli

import (
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/config"
	"github.com/edotau/claude-code/internal/helptext"
)

const configUsage = `usage: claude-code config show
       claude-code config edit
show  print the vault's pins, env, overlays and key_command as JSON;
      secrets never appear (use claude-code secrets)
edit  $VISUAL/$EDITOR on that JSON; saved in one transaction,
      refused if the vault changed meanwhile`

// configVerbs is the config subcommand table; Phase 4 adds migrate|finalize|rollback|rotate-key rows.
var configVerbs = map[string]func(args []string) int{"show": configShow, "edit": configEdit}

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
