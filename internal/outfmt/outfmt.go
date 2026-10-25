// Package outfmt is the one output-format convention every diagnostic verb shares: --format text|json,
// data on stdout, diagnostics on stderr.
package outfmt

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
)

const (
	Text = "text"
	JSON = "json"
)

// Flag registers --format on fs, defaulting to text.
func Flag(fs *flag.FlagSet) *string {
	return fs.String("format", Text, "output format: text|json")
}

// Check rejects anything but text or json.
func Check(v string) error {
	if v != Text && v != JSON {
		return fmt.Errorf("--format %q: want text or json", v)
	}
	return nil
}

// Write encodes v as indented JSON followed by a newline.
func Write(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// Parse is fs.Parse with flags allowed after positionals (`profile gemini --format json`); returns the positionals.
// Everything from the first literal "--" onward is positional, never flag-parsed: fs.Parse itself swallows a
// "--" it meets mid-stream, so a second "--"-check after the fact (on its own) misses "-h" two tokens later.
func Parse(fs *flag.FlagSet, args []string) ([]string, error) {
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
