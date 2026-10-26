package diff

import (
	"strings"
)

// splitPositional separates positional args from flags so Go's flag.Parse (which stops at the
// first non-flag token) can still handle argparse-style interleaving like ". --base main".
func splitPositional(args []string, boolFlags map[string]bool) (positional, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) > 0 && a[0] == '-' && a != "-" {
			rest = append(rest, a)
			name := strings.TrimLeft(a, "-")
			if strings.Contains(name, "=") || boolFlags[name] {
				continue
			}
			if i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
			continue
		}
		positional = append(positional, a)
	}
	return positional, rest
}
