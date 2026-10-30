package databricks

import (
	"fmt"
	"strings"
)

// removedFlags maps each flag Phase 6 removed to its hint; they stay errors, never aliases.
var removedFlags = map[string]string{
	"--provider": "was renamed to --profile",
	"--env":      "was renamed to --profile",
	"--tier":     "was replaced by --model <alias>",
	"--router":   "was removed; routing is automatic (see `claude-code gateway router`)",
	"--1m":       "was removed; 1M context is the base behavior on serving and first-party endpoints",
}

// RemovedFlagHint returns the error for a removed flag in args (up to "--"), or nil.
func RemovedFlagHint(args []string) error {
	for _, a := range args {
		if a == "--" {
			return nil
		}
		name, _, _ := strings.Cut(a, "=")
		if hint, ok := removedFlags[name]; ok {
			return fmt.Errorf("%s %s", name, hint)
		}
	}
	return nil
}
