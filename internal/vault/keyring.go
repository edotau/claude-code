// The keyring plane (this file) is the OS secret-service plane: a shell-out to `secret-tool` (libsecret) on Linux,
// `security` on macOS. Degrades rather than failing hard: no tool, no session, or a locked keyring
// all read as "this plane holds nothing", so callers fall through to HARNESS_VAULT_KEY or key_command.

package vault

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/edotau/claude-code/internal/audit"
)

// service labels every account this harness stores (the service name for secret-tool, the
// -s argument for `security`).
const service = "claude-code"

// timeout bounds one keyring call; a locked keyring can otherwise block on an unlock prompt
// with no agent to answer it.
var timeout = 3 * time.Second

// downFor is how long one keyring timeout fast-fails later calls before the next call re-probes the tool.
var downFor = time.Minute

// downLatch fast-fails keyring calls for downFor after a timeout, so a loop over many names
// (`configure provider --list`) pays the timeout once, and a long-lived process recovers after it.
type downLatch struct{ until atomic.Int64 }

// Store(true) opens the fast-fail window from now; Store(false) closes it.
func (l *downLatch) Store(on bool) {
	if !on {
		l.until.Store(0)
		return
	}
	l.until.Store(time.Now().Add(downFor).UnixNano())
}

// Load reports whether the fast-fail window is still open.
func (l *downLatch) Load() bool { return time.Now().UnixNano() < l.until.Load() }

var down downLatch

// ErrKeyringNoAnswer means the tool timed out or the `down` latch was already set — not a plain miss.
var ErrKeyringNoAnswer = errors.New("keyring: did not answer")

// ErrKeyringUnavailable means the tool ran and failed for a reason other than "not stored" (a genuine
// keyring failure, not a positively established absence).
var ErrKeyringUnavailable = errors.New("keyring: unavailable")

// errKeyringNoTool means no keyring tool is on PATH, or this OS has none wired up (Windows). Wraps
// ErrKeyringUnavailable so errors.Is(err, ErrKeyringUnavailable) still matches; callers that must
// treat "no tool ever existed" differently from "an installed tool just failed" (R4) check this
// more specific sentinel first.
var errKeyringNoTool = fmt.Errorf("keyring: no tool on PATH for this OS: %w", ErrKeyringUnavailable)

// keyringGOOS picks the keyring plane's OS branch; tests swap it to run every branch on any host.
var keyringGOOS = runtime.GOOS

// KeyringTool returns the plane's binary name for this OS, or "" where none is wired up.
func KeyringTool() string {
	switch keyringGOOS {
	case "linux":
		return "secret-tool"
	case "darwin":
		return "security"
	default:
		return ""
	}
}

// KeyringAvailable reports whether a keyring tool is on PATH for this OS; HARNESS_KEYRING=off forces false.
func KeyringAvailable() bool {
	if os.Getenv("HARNESS_KEYRING") == "off" {
		return false
	}
	b := KeyringTool()
	if b == "" {
		return false
	}
	_, err := exec.LookPath(b)
	return err == nil
}

// KeyringLookup looks up name in the OS keyring. err is ErrKeyringNoAnswer when the tool timed out or the
// `down` latch is already set, errKeyringNoTool when no tool is on PATH for this OS; a plain,
// positively-established miss reads as ("", false, nil).
func KeyringLookup(name string) (value string, found bool, err error) {
	if name == "" {
		return "", false, nil
	}
	if !KeyringAvailable() {
		return "", false, errKeyringNoTool
	}
	if down.Load() {
		return "", false, ErrKeyringNoAnswer
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := lookupCommand(ctx, name)
	cmd.WaitDelay = 250 * time.Millisecond
	out, err2 := cmd.Output()
	if ctx.Err() != nil {
		down.Store(true)
		return "", false, ErrKeyringNoAnswer
	}
	if err2 != nil {
		return "", false, lookupFailure(err2)
	}
	v := strings.TrimSpace(string(out))
	return v, v != "", nil
}

// lookupCommand is this OS's keyring read of name's value.
func lookupCommand(ctx context.Context, name string) *exec.Cmd {
	switch keyringGOOS {
	case "linux":
		return exec.CommandContext(ctx, "secret-tool", "lookup", "service", service, "account", name)
	case "darwin":
		return exec.CommandContext(ctx, "security", "find-generic-password", "-a", name, "-s", service, "-w")
	}
	return nil
}

// lookupFailure maps a failed lookup to nil (a positively established absence) or ErrKeyringUnavailable.
func lookupFailure(err error) error {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return ErrKeyringUnavailable
	}
	stderr := strings.TrimSpace(string(exit.Stderr))
	switch {
	case keyringGOOS == "linux" && exit.ExitCode() == 1 && stderr == "":
		return nil // only a positively established absence
	case keyringGOOS == "darwin" && exit.ExitCode() == 44:
		return nil // only a positively established absence
	case stderr != "":
		return fmt.Errorf("%w: %s", ErrKeyringUnavailable, stderr)
	}
	return ErrKeyringUnavailable
}

// osKeyringSet stores value under name, overwriting any existing entry. The value always goes on stdin, never
// argv: an argv secret is visible to every same-uid process (`ps`) for the life of the call.
func osKeyringSet(name, value string) error {
	if err := checkKeyringSet(name, value); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*timeout)
	defer cancel()
	cmd := storeCommand(ctx, name, value)
	cmd.WaitDelay = 250 * time.Millisecond
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		down.Store(true)
	}
	// The tool's output carries the reason (unlock refused, no D-Bus); `security -i` exits 0 even on failure.
	msg := strings.TrimSpace(strings.ReplaceAll(string(out), value, "<redacted>"))
	if err := storeFailure(name, msg, err); err != nil {
		return err
	}
	audit.Log(audit.SecretSet, name, "keyring") // the name only, never the value
	return nil
}

// checkKeyringSet refuses a store before any tool runs: bad name or value, no tool, or a keyring already down.
func checkKeyringSet(name, value string) error {
	if name == "" {
		return errors.New("keyring: empty name")
	}
	if value == "" {
		return fmt.Errorf("keyring: refusing to store an empty value for %s", name)
	}
	// A newline in either half would end the `security -i` command early and run the rest as another.
	if strings.ContainsAny(name+value, "\r\n") {
		return fmt.Errorf("keyring: %s: name or value spans more than one line", name)
	}
	if !KeyringAvailable() {
		return fmt.Errorf("keyring: no keyring tool on PATH for %s — set HARNESS_VAULT_KEY or meta/key_command instead", keyringGOOS)
	}
	if down.Load() {
		return errors.New("keyring: the keyring did not answer earlier this run — set HARNESS_VAULT_KEY or meta/key_command instead")
	}
	return nil
}

// storeCommand is this OS's keyring write of name, with value fed on stdin.
func storeCommand(ctx context.Context, name, value string) *exec.Cmd {
	var cmd *exec.Cmd
	switch keyringGOOS {
	case "linux":
		cmd = exec.CommandContext(ctx, "secret-tool", "store", "--label="+service, "service", service, "account", name)
		cmd.Stdin = strings.NewReader(value)
	case "darwin":
		// `security -i` reads its command line from stdin, so -w's value never reaches argv.
		cmd = exec.CommandContext(ctx, "security", "-i")
		cmd.Stdin = strings.NewReader("add-generic-password -U -a " + quote(name) + " -s " + service +
			" -w " + quote(value) + "\n")
	}
	return cmd
}

// storeFailure turns a store's exit error and redacted output into the error osKeyringSet returns, nil on success.
func storeFailure(name, msg string, err error) error {
	switch {
	case err != nil && msg != "":
		return fmt.Errorf("keyring: store %s: %w: %s", name, err, msg)
	case err != nil:
		return fmt.Errorf("keyring: store %s: %w", name, err)
	case msg != "":
		return fmt.Errorf("keyring: store %s: %s", name, msg)
	}
	return nil
}

// quote wraps s for the `security -i` tokenizer: double quotes, with backslash and quote escaped.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
