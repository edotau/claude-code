package diff

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
)

// execCommand runs a command with list args (never a shell string); stdout only, matching subprocess.run.
func execCommand(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	return string(out), err
}

func absPath(p string) (string, error) {
	return filepath.Abs(p)
}

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
