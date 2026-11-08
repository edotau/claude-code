package completion

import "github.com/edotau/claude-code/internal/ui/helptext"

// Plain is a free-text flag.
func Plain(name string) Flag { return Flag{Name: name} }

// BoolFlag is a flag that takes no value.
func BoolFlag(name string) Flag { return Flag{Name: name, IsBool: true} }

// ValuedFlag is a flag whose value comes from values.
func ValuedFlag(name string, values func() []string) Flag { return Flag{Name: name, Values: values} }

// FormatFlag is the shared --format text|json flag.
func FormatFlag() Flag {
	return ValuedFlag("format", func() []string { return []string{helptext.Text, helptext.JSON} })
}
