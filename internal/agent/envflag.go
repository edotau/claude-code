package agent

import (
	"errors"
	"fmt"
	"strings"

	"github.com/edotau/claude-code/internal/proc"
)

// validEnvName reports why name cannot be set with --env: credential-shaped and harness-scrubbed names are refused.
// The error names NAME only; a value never appears in it.
func validEnvName(name string) error {
	if name == "" || strings.ContainsAny(name, " \t") {
		return errors.New("--env: want NAME=VALUE")
	}
	up := strings.ToUpper(name)
	reserved := proc.EnvBlocked(name) || proc.EnvBlocked(up)
	if strings.HasSuffix(up, "_BASE_URL") && !strings.HasPrefix(up, "HARNESS_") {
		reserved = false // a route override such as ANTHROPIC_BASE_URL is not a secret
	}
	switch {
	case reserved:
		return fmt.Errorf("--env %s is reserved by the harness credential scrub: use `claude-code vault set` or a provider's auth.command", name)
	case strings.HasSuffix(up, "_API_KEY"), strings.HasSuffix(up, "_TOKEN"), strings.HasSuffix(up, "_SECRET"), strings.Contains(up, "AUTH"):
		return fmt.Errorf("--env %s looks like a credential: store it with `claude-code vault set` or a provider's auth.command, not on the command line", name)
	}
	return nil
}

// ValidateEnv checks every KEY=VALUE entry of --env; errors never echo a value.
func ValidateEnv(env []string) error {
	for _, kv := range env {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			return errors.New("--env: want NAME=VALUE")
		}
		if err := validEnvName(name); err != nil {
			return err
		}
	}
	return nil
}
