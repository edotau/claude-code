package helptext

// Plain is a free-text flag.
func Plain(name string) Flag { return Flag{Name: name} }

// BoolFlag is a flag that takes no value.
func BoolFlag(name string) Flag { return Flag{Name: name, IsBool: true} }

// ValuedFlag is a flag whose value comes from values.
func ValuedFlag(name string, values func() []string) Flag { return Flag{Name: name, Values: values} }

// CompletionFormatFlag is the shared --format text|json flag.
func CompletionFormatFlag() Flag {
	return ValuedFlag("format", func() []string { return []string{Text, JSON} })
}
