package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/egress"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/redact"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/textutil"
)

// childWaitDelay is how long a killed child's open pipes may delay Wait; a var so tests can lengthen it.
var childWaitDelay = 5 * time.Second

// ChildCmd builds an exec.Cmd in its own process group with inherited credentials scrubbed; claude keeps the vault key.
func ChildCmd(ctx context.Context, bin string, argv, env []string, dir, stdin string, keepEnv []string, cli bool) (*exec.Cmd, *retry.TailBuffer) {
	cmd := exec.CommandContext(ctx, bin, argv...)
	proc.OwnGroup(cmd)
	cmd.WaitDelay = childWaitDelay
	mode := EnvClaude
	if cli {
		mode = EnvAgent
	}
	cmd.Env = EnvSpec{Mode: mode, Keep: keepEnv, Set: env, PWD: dir}.Environ(ctx) // launch values after the scrub
	cmd.Dir = dir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	tail := &retry.TailBuffer{}
	cmd.Stderr = io.Writer(tail) // the dispatch log, when ctx carries one, sees stderr as it arrives
	if sk := wire.StderrLogFrom(ctx); sk != nil {
		cmd.Stderr = io.MultiWriter(tail, sk.Stream())
	}
	return cmd, tail
}

// ChildErr names a failed child by its stderr tail; a deadline surfaces as ctx.Err for classification.
func ChildErr(ctx context.Context, name string, err error, tail *retry.TailBuffer) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w", name, ctx.Err())
	}
	if t := stderrTail(tail.String()); t != "" {
		return fmt.Errorf("%s: %w: %s", name, err, t)
	}
	return fmt.Errorf("%s: %w", name, err)
}

// stderrTailBytes caps the stderr an error quotes.
const stderrTailBytes = 600

// stderrTail is a child's stderr fit for an error: trimmed, its last stderrTailBytes, secrets redacted.
func stderrTail(s string) string {
	// mask before the cut: a secret straddling it would leave a suffix no needle matches
	t := redact.Text(textutil.RedactSecrets(strings.TrimSpace(s))) // shapes first, then vault values incl. the env master key
	if len(t) > stderrTailBytes {
		t = "…" + strings.ToValidUTF8(t[len(t)-stderrTailBytes:], "") // the cut may split a rune
	}
	return t
}

// RunValues is the run's values (values, else ctx's snapshot, else one of the process env) set on ctx, plus a copy.
func RunValues(ctx context.Context, values proc.Values) (context.Context, proc.Values) {
	if values == nil {
		var ok bool
		values, ok = proc.ValuesFromContext(ctx)
		if !ok {
			values = proc.ValuesFromEnv(os.Environ())
			if values[route.OverlayVar] == "" { // an ambient empty overlay must not suppress the stored one
				delete(values, route.OverlayVar)
			}
			values = withAncestorDispatch(values) // an env wipe inside a leaf must not make a top-level dispatch
		}
	}
	return proc.WithValues(ctx, values), values.Merge(nil)
}

// runEnviron preserves inherited entry order while applying only the run snapshot's values.
func runEnviron(ctx context.Context) []string {
	base := os.Environ()
	values, ok := proc.ValuesFromContext(ctx)
	if !ok {
		return base
	}
	if host := egress.HostContext(ctx); host != values[egress.EnvHost] {
		values = values.With(egress.EnvHost, host)
	}
	out := make([]string, 0, len(values))
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if value, found := values[name]; found {
			out = append(out, name+"="+value)
			delete(values, name)
		}
	}
	return append(out, values.Env()...)
}

// valuesContext restores a supplied snapshot without replacing more specific request context values.
func valuesContext(ctx context.Context, values proc.Values) context.Context {
	if values != nil {
		return proc.WithValues(ctx, values)
	}
	return ctx
}

// keepSet unions explicit names with the run's inherited scrub exceptions.
func keepSet(ctx context.Context, names []string) map[string]bool {
	keep := make(map[string]bool, len(names))
	for _, name := range names {
		keep[name] = true
	}
	for name := range strings.SplitSeq(proc.Value(ctx, "HARNESS_KEEP_ENV"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			keep[name] = true
		}
	}
	return keep
}
