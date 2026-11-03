package helptext

import "flag"

// Explicit is true when --<name> appeared on the command line fs parsed, so it beats any computed default.
func Explicit(fs *flag.FlagSet, name string) bool {
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			explicit = true
		}
	})
	return explicit
}

// ExtractBool pulls a `--name` boolean flag out of args wherever it appears (NAME --flag and
// --flag NAME must both work — the stdlib `flag` package only accepts flags before positionals).
func ExtractBool(args []string, name string) (bool, []string) {
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
