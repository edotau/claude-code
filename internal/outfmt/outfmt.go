// Package outfmt is the one output-format convention every diagnostic verb shares: --format text|json,
// data on stdout, diagnostics on stderr.
package outfmt

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
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
func Parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos, args = append(pos, args[0]), args[1:]
	}
}
