package cli

import (
	"flag"
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/profile"
	"github.com/edotau/claude-code/internal/providers"
)

var profilePage = helptext.Page{
	Usage: "claude-code profile <command> [flags] [<name>]",
	About: "Per-agent account bindings. With no profile active nothing changes.",
	Commands: [][2]string{
		{"list", "set names; * marks the active one"},
		{"show", "per-agent table of a set (default: active) and env drift"},
		{"use", "make a set active (relaunch to apply)"},
		{"off", "deactivate; sets are kept"},
		{"save", "snapshot today's bindings as a set (--force)"},
		{"migrate", "fold databricks-<env> rows into <env>, sets into DEFAULT"},
	},
	Hint: "claude-code profile <command> -h lists that command's flags.",
}

func cmdProfile(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		profilePage.Fprint(os.Stderr)
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return profileList(rest)
	case "show":
		return profileShow(rest)
	case "use":
		return profileUse(rest)
	case "off":
		return profileOff(rest)
	case "save":
		return profileSave(rest)
	case "migrate":
		return profileMigrate(rest)
	}
	fail("profile: unknown subcommand %q", sub)
	profilePage.Fprint(os.Stderr)
	return 2
}

// parseNamed parses flags around at most one positional name (flags may follow it).
func parseNamed(fs *flag.FlagSet, args []string) (name string, code int, done bool) {
	if code, done = helptext.Parse(fs, args); done {
		return "", code, true
	}
	if fs.NArg() > 0 {
		name = fs.Arg(0)
		if code, done = helptext.Parse(fs, fs.Args()[1:]); done {
			return "", code, true
		}
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "claude-code: %s: unexpected argument %q\n", fs.Name(), fs.Arg(0))
		return "", 2, true
	}
	return name, 0, false
}

func profileList(args []string) int {
	fs := helptext.NewFlagSet("profile list", helptext.Page{Usage: "claude-code profile list [flags]"})
	format := outfmt.Flag(fs)
	if _, code, done := parseNamed(fs, args); done {
		return code
	}
	reg, code := loadReg(*format)
	if reg == nil {
		return code
	}
	if *format == outfmt.JSON {
		return writeJSON(reg.Profiles)
	}
	profile.List(os.Stdout, reg)
	return 0
}

func profileShow(args []string) int {
	fs := helptext.NewFlagSet("profile show", helptext.Page{Usage: "claude-code profile show [flags] [<name>]"})
	format := outfmt.Flag(fs)
	name, code, done := parseNamed(fs, args)
	if done {
		return code
	}
	reg, code := loadReg(*format)
	if reg == nil {
		return code
	}
	if name == "" {
		if name = reg.Profiles.Active; name == "" {
			fmt.Println("no profile active: launches use today's selection")
			return 0
		}
	}
	if *format == outfmt.JSON {
		set, ok := reg.Profiles.Sets[name]
		if !ok {
			return fail("no profile set %q", name)
		}
		return writeJSON(map[string]any{"name": name, "active": name == reg.Profiles.Active, "agents": set,
			"drift": profile.Drift(reg)})
	}
	if err := profile.Show(os.Stdout, reg, name); err != nil {
		return fail("%v", err)
	}
	return 0
}

func profileUse(args []string) int {
	fs := helptext.NewFlagSet("profile use", helptext.Page{Usage: "claude-code profile use [flags] <name>"})
	dry := fs.Bool("dry-run", false, "print the overlay diff, write nothing")
	name, code, done := parseNamed(fs, args)
	if done {
		return code
	}
	if name == "" {
		fs.Usage()
		return 2
	}
	return applyProfile(*dry, fmt.Sprintf("profile active: %s (relaunch to apply)", name),
		func(apply bool) ([]byte, []byte, error) { return profile.Use(apply, name) })
}

func profileOff(args []string) int {
	fs := helptext.NewFlagSet("profile off", helptext.Page{Usage: "claude-code profile off [flags]"})
	dry := fs.Bool("dry-run", false, "print the overlay diff, write nothing")
	if _, code, done := parseNamed(fs, args); done {
		return code
	}
	return applyProfile(*dry, "profile off: launches use today's selection", profile.Off)
}

// applyProfile runs a use/off change: --dry-run prints the overlay diff, otherwise it writes and prints done.
func applyProfile(dry bool, done string, change func(apply bool) (oldDoc, newDoc []byte, err error)) int {
	oldDoc, newDoc, err := change(!dry)
	if err != nil {
		return fail("%v", err)
	}
	if dry {
		profile.PrintDiff(os.Stdout, oldDoc, newDoc)
		return 0
	}
	fmt.Println(done)
	return 0
}

func profileSave(args []string) int {
	fs := helptext.NewFlagSet("profile save", helptext.Page{Usage: "claude-code profile save [flags] <name>"})
	force := fs.Bool("force", false, "overwrite an existing set")
	name, code, done := parseNamed(fs, args)
	if done {
		return code
	}
	if name == "" {
		fs.Usage()
		return 2
	}
	reg, err := providers.Load()
	if err != nil {
		return fail("%v", err)
	}
	set, err := profile.Snapshot(reg)
	if err != nil {
		return fail("%v", err)
	}
	if err := profile.Save(name, set, *force); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("profile saved: %s (use it with `claude-code profile use %s`)\n", name, name)
	return 0
}

func profileMigrate(args []string) int {
	fs := helptext.NewFlagSet("profile migrate", helptext.Page{
		Usage: "claude-code profile migrate [flags]",
		About: "Folds databricks-<env>[-oai] into a preset row named <env>, replaces the\n" +
			"databricks-<env> sets with DEFAULT, keeps own. Dry run unless --apply.",
	})
	apply := fs.Bool("apply", false, "write the vault overlay (default: print the diff)")
	if _, code, done := parseNamed(fs, args); done {
		return code
	}
	oldDoc, newDoc, notes, err := profile.Migrate(*apply)
	if err != nil {
		return fail("profile migrate: %v", err)
	}
	for _, n := range notes {
		fmt.Println("# " + n)
	}
	if !profile.PrintDiff(os.Stdout, oldDoc, newDoc) {
		fmt.Println("# overlay unchanged")
	}
	if !*apply {
		fmt.Println("# dry run: nothing written (--apply to write)")
	}
	return 0
}

// loadReg validates the output format and loads the registry; a nil registry means exit with the returned code.
func loadReg(format string) (*providers.Registry, int) {
	if err := outfmt.Check(format); err != nil {
		return nil, fail("%v", err)
	}
	reg, err := providers.Load()
	if err != nil {
		return nil, fail("%v", err)
	}
	return reg, 0
}

func writeJSON(v any) int {
	if err := outfmt.Write(os.Stdout, v); err != nil {
		return fail("%v", err)
	}
	return 0
}
