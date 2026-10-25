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
// Everything from the first literal "--" onward is positional, never flag-parsed: fs.Parse itself swallows a
// "--" it meets mid-stream, so a second "--"-check after the fact (on its own) misses "-h" two tokens later.
func Parse(fs *flag.FlagSet, args []string) ([]string, error) {
	head, tail := args, []string(nil)
	for i, a := range args {
		if a == "--" {
			head, tail = args[:i], args[i+1:]
			break
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
