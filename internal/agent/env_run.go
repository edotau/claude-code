package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/egress"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/redact"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/ui/helptext"
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
	t := redact.TextSecrets(strings.TrimSpace(s)) // shapes first, then vault values incl. the env master key
	if len(t) > stderrTailBytes {
		t = "…" + strings.ToValidUTF8(t[len(t)-stderrTailBytes:], "") // the cut may split a rune
	}
	return t
}

// RunValues snapshots the run values, projecting trusted dispatch identity and policy before ancestry fallback.
func RunValues(ctx context.Context, values proc.Values) (context.Context, proc.Values) {
	fromEnv := false
	if values == nil {
		var ok bool
		values, ok = proc.ValuesFromContext(ctx)
		if !ok {
			fromEnv = true
			values = proc.ValuesFromEnv(os.Environ())
			if values[route.OverlayVar] == "" { // an ambient empty overlay must not suppress the stored one
				delete(values, route.OverlayVar)
			}
		}
	} else {
		values = values.Merge(nil)
	}
	if id, ok := ctx.Value(dispatchIDKey{}).(string); ok {
		depth, _ := ctx.Value(dispatchDepthKey{}).(int)
		for k := range values {
			if proc.EnvNameEqual(k, proc.DispatchIDEnv) || proc.EnvNameEqual(k, proc.DispatchDepthEnv) {
				delete(values, k) // a case variant must not coexist with the harness's spelling
			}
		}
		values[proc.DispatchIDEnv], values[proc.DispatchDepthEnv] = id, strconv.Itoa(depth)
	}
	if fromEnv {
		values = withAncestorDispatch(values) // an env wipe inside a leaf must not make a top-level dispatch
	}
	for name := range values {
		if proc.EnvNameEqual(name, ResumeNonceEnv) {
			delete(values, name) // adoption reads the ambient capability once; run snapshots must not re-inject it
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
	canon := make(map[string]string, len(values)) // folded name -> the one spelling that wins: upper-case, else first sorted
	for _, k := range slices.Sorted(maps.Keys(values)) {
		if cur, ok := canon[proc.EnvFold(k)]; !ok || k == proc.EnvFold(k) && cur != k {
			canon[proc.EnvFold(k)] = k
		}
	}
	for k := range values {
		if canon[proc.EnvFold(k)] != k {
			delete(values, k)
		}
	}
	out := make([]string, 0, len(values))
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if key, ok := canon[proc.EnvFold(name)]; ok {
			if value, found := values[key]; found {
				out = append(out, key+"="+value)
				delete(values, key)
			}
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
	raw := proc.Value(ctx, "HARNESS_KEEP_ENV")
	if values, ok := proc.ValuesFromContext(ctx); ok && raw == "" {
		for k, v := range values {
			if proc.EnvNameEqual(k, "HARNESS_KEEP_ENV") {
				raw = v
			}
		}
	}
	for name := range strings.SplitSeq(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			keep[name] = true
		}
	}
	return keep
}

// RunOwnLogin runs a vendor CLI in the cwd on login's own sign-in only (other credentials and unset names dropped).
func RunOwnLogin(ctx context.Context, login, bin string, unset, argv []string) int {
	dir, err := os.Getwd()
	if err != nil {
		return helptext.Fail("%s: %v", login, err)
	}
	ctx = ProbeContext(ctx, login)
	cmd := exec.CommandContext(ctx, bin, argv...)
	own := map[string]bool{}
	for _, n := range proc.OwnLoginEnv[login] {
		own[n] = true
	}
	env := EnvSpec{Mode: EnvAgent, Unset: unset, PWD: dir}.Environ(ctx)
	// no HARNESS_KEEP_ENV exception applies: every credential-shaped name goes except the login's own
	cmd.Env = proc.Drop(env, own, proc.ScrubDenyCredentials)
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = dir, os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode()
	}
	if err != nil {
		return helptext.Fail("%s: %v", login, err)
	}
	return 0
}
