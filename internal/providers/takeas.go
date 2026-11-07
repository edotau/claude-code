package providers

import (
	"errors"
	"fmt"
	"strings"
)

// TakeAs removes --as user|sp (or --as=V) from args and validates it; it stops at a bare "--".
func TakeAs(args []string) (string, []string, error) {
	var as string
	var rest []string
	seen := false
	for i := 0; i < len(args); i++ {
		name, val, hasVal := strings.Cut(args[i], "=")
		switch {
		case args[i] == "--":
			rest = append(rest, args[i:]...)
			i = len(args)
		case name != "--as":
			rest = append(rest, args[i])
		case hasVal:
			as, seen = val, true
		case i+1 < len(args):
			i++
			as, seen = args[i], true
		default:
			return "", nil, errors.New("--as needs user or sp")
		}
	}
	if err := checkAs(as, seen); err != nil {
		return "", nil, err
	}
	return as, rest, nil
}

// checkAs rejects a given --as value other than user or sp.
func checkAs(as string, seen bool) error {
	if seen && as != IdentityUser && as != IdentitySP {
		return fmt.Errorf("--as %q must be user or sp", as)
	}
	return nil
}
