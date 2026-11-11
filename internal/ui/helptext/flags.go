package helptext

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// WantsHelp reports a -h/-help/--help token before any literal "--" terminator, so a caller can route
// explicit help to stdout itself instead of letting fs.Parse's automatic usage call land on stderr.
func WantsHelp(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "-h" || a == "-help" || a == "--help" {
			return true
		}
	}
	return false
}

// ExitCode maps a FlagSet parse error to the verb's exit code: 0 after -h, 2 for a bad flag.
func ExitCode(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}

// Parse parses args into fs: (0,false) to continue, (0,true) after -h, (2,true) on a bad flag.
func Parse(fs *flag.FlagSet, args []string) (code int, done bool) {
	if err := fs.Parse(args); err != nil {
		return ExitCode(err), true
	}
	return 0, false
}

// NewFlagSet builds a ContinueOnError FlagSet on stderr whose -h renders p with p.Flags set to it.
func NewFlagSet(name string, p Page) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		p.Flags = fs
		p.Fprint(fs.Output())
	}
	return fs
}

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

// Fail prints a one-line error and returns exit code 1.
func Fail(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "claude-code: "+format+"\n", a...)
	return 1
}

// FailUsage prints a one-line error and returns usage exit code 2.
func FailUsage(format string, a ...any) int {
	Fail(format, a...)
	return 2
}

// WriteJSONOrFail writes v as indented JSON to stdout; a write error is reported under label and returns 1.
func WriteJSONOrFail(label string, v any) int {
	if err := WriteJSON(os.Stdout, v); err != nil {
		return Fail("%s: %v", label, err)
	}
	return 0
}

// BadFormat reports an invalid --format value as a usage error (exit 2).
func BadFormat(prefix string, err error) int {
	if prefix == "" {
		return FailUsage("%v", err)
	}
	return FailUsage(prefix+": %v", err)
}

const (
	Text = "text"
	JSON = "json"
)

// FormatFlag registers --format on fs, defaulting to text.
func FormatFlag(fs *flag.FlagSet) *string {
	return fs.String("format", Text, "output format: text|json")
}

// CheckFormat rejects anything but text or json.
func CheckFormat(v string) error {
	if v != Text && v != JSON {
		return fmt.Errorf("--format %q: want text or json", v)
	}
	return nil
}

// WriteJSON encodes v as indented JSON followed by a newline.
func WriteJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// ParseArgs is fs.Parse with flags allowed after positionals (`profile gemini --format json`); returns the positionals.
// Everything from the first literal "--" onward is positional, never flag-parsed: fs.Parse itself swallows a
// "--" it meets mid-stream, so a second "--"-check after the fact (on its own) misses "-h" two tokens later.
func ParseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	head, tail := args, []string(nil)
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			head, tail = args[:i], args[i+1:]
			break
		}
		if consumesValue(fs, args[i]) {
			i++ // the next token is this flag's value, e.g. "--output --"; it can't be the terminator
		}
	}
	var pos []string
	for {
		if err := fs.Parse(head); err != nil {
			return nil, err
		}
		head = fs.Args()
		if len(head) == 0 {
			return append(pos, tail...), nil
		}
		pos, head = append(pos, head[0]), head[1:]
	}
}

// consumesValue reports whether a is a registered, non-bool flag given without "=": fs.Parse consumes the
// very next token as its value, so the terminator scan above must skip that token too.
func consumesValue(fs *flag.FlagSet, a string) bool {
	if len(a) < 2 || a[0] != '-' {
		return false
	}
	name := strings.TrimLeft(a, "-")
	if name == "" || strings.Contains(name, "=") {
		return false
	}
	f := fs.Lookup(name)
	if f == nil {
		return false
	}
	if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
		return false
	}
	return true
}

// removedFlags maps each flag removed in the profile rename to its hint; they stay errors, never aliases.
var removedFlags = map[string]string{
	"--provider": "was renamed to --profile",
	"--env":      "was renamed to --profile",
	"--tier":     "was replaced by --model <alias>",
	"--router":   "was removed; routing is automatic (see `claude-code gateway`)",
	"--1m":       "was removed; 1M context is the base behavior on serving and first-party endpoints",
}

// RemovedFlagHint returns the error for a removed flag in args, or nil; the scan stops at "--" or at the first
// bare word that is not a flag's value (a claude subcommand such as `mcp add --env` owns the flags after it).
func RemovedFlagHint(args []string) error {
	for i, a := range args {
		if a == "--" || (!strings.HasPrefix(a, "-") && (i == 0 || !strings.HasPrefix(args[i-1], "-"))) {
			return nil
		}
		name, _, _ := strings.Cut(a, "=")
		if hint, ok := removedFlags[name]; ok {
			return fmt.Errorf("%s %s", name, hint)
		}
	}
	return nil
}
