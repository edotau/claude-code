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

// down latches once a lookup/store has already timed out this process, so a loop over many
// names (`configure provider --list`) pays the timeout once, not once per name.
var down atomic.Bool

// ErrKeyringNoAnswer means the tool timed out or the `down` latch was already set — not a plain miss.
var ErrKeyringNoAnswer = errors.New("keyring: did not answer")

// ErrKeyringUnavailable means the tool ran and failed for a reason other than "not stored" (a genuine
// keyring failure, not a positively established absence).
var ErrKeyringUnavailable = errors.New("keyring: unavailable")

// ErrKeyringNoTool means no keyring tool is on PATH, or this OS has none wired up (Windows). Wraps
// ErrKeyringUnavailable so errors.Is(err, ErrKeyringUnavailable) still matches; callers that must
// treat "no tool ever existed" differently from "an installed tool just failed" (R4) check this
// more specific sentinel first.
var ErrKeyringNoTool = fmt.Errorf("keyring: no tool on PATH for this OS: %w", ErrKeyringUnavailable)

// KeyringTool returns the plane's binary name for this OS, or "" where none is wired up.
func KeyringTool() string {
	switch runtime.GOOS {
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
// `down` latch is already set, ErrKeyringNoTool when no tool is on PATH for this OS; a plain,
// positively-established miss reads as ("", false, nil).
func KeyringLookup(name string) (value string, found bool, err error) {
	if name == "" {
		return "", false, nil
	}
	if !KeyringAvailable() {
		return "", false, ErrKeyringNoTool
	}
	if down.Load() {
		return "", false, ErrKeyringNoAnswer
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.CommandContext(ctx, "secret-tool", "lookup", "service", service, "account", name)
	case "darwin":
		cmd = exec.CommandContext(ctx, "security", "find-generic-password", "-a", name, "-s", service, "-w")
	}
	cmd.WaitDelay = 250 * time.Millisecond
	out, err2 := cmd.Output()
	if ctx.Err() != nil {
		down.Store(true)
		return "", false, ErrKeyringNoAnswer
	}
	if err2 != nil {
		var exit *exec.ExitError
		stderr := ""
		if errors.As(err2, &exit) {
			stderr = strings.TrimSpace(string(exit.Stderr))
		}
		switch runtime.GOOS {
		case "linux":
			if errors.As(err2, &exit) && exit.ExitCode() == 1 && stderr == "" {
				return "", false, nil // only a positively established absence
			}
		case "darwin":
			if errors.As(err2, &exit) && exit.ExitCode() == 44 {
				return "", false, nil // only a positively established absence
			}
		}
		if stderr != "" {
			return "", false, fmt.Errorf("%w: %s", ErrKeyringUnavailable, stderr)
		}
		return "", false, ErrKeyringUnavailable
	}
	v := strings.TrimSpace(string(out))
	return v, v != "", nil
}

// keyringGet is KeyringLookup's "found?" shape, the API most callers want: ErrKeyringNoAnswer reads as a miss too.
func keyringGet(name string) (string, bool) {
	v, found, _ := KeyringLookup(name)
	return v, found
}

// KeyringSet stores value under name, overwriting any existing entry. The value always goes on stdin, never
// argv: an argv secret is visible to every same-uid process (`ps`) for the life of the call.
func KeyringSet(name, value string) error {
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
		return fmt.Errorf("keyring: no keyring tool on PATH for %s — set HARNESS_VAULT_KEY or meta/key_command instead", runtime.GOOS)
	}
	if down.Load() {
		return errors.New("keyring: the keyring did not answer earlier this run — set HARNESS_VAULT_KEY or meta/key_command instead")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*timeout)
	defer cancel()
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.CommandContext(ctx, "secret-tool", "store", "--label="+service, "service", service, "account", name)
		cmd.Stdin = strings.NewReader(value)
	case "darwin":
		// `security -i` reads its command line from stdin, so -w's value never reaches argv.
		cmd = exec.CommandContext(ctx, "security", "-i")
		cmd.Stdin = strings.NewReader("add-generic-password -U -a " + quote(name) + " -s " + service +
			" -w " + quote(value) + "\n")
	}
	cmd.WaitDelay = 250 * time.Millisecond
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		down.Store(true)
	}
	// The tool's output carries the reason (unlock refused, no D-Bus); `security -i` exits 0 even on failure.
	msg := strings.TrimSpace(strings.ReplaceAll(string(out), value, "<redacted>"))
	switch {
	case err != nil && msg != "":
		return fmt.Errorf("keyring: store %s: %w: %s", name, err, msg)
	case err != nil:
		return fmt.Errorf("keyring: store %s: %w", name, err)
	case msg != "":
		return fmt.Errorf("keyring: store %s: %s", name, msg)
	}
	audit.Log(audit.SecretSet, name, "keyring") // the name only, never the value
	return nil
}

// KeyringDelete removes name from the keyring; a name that is not stored is not an error, a refused delete is.
func KeyringDelete(name string) error {
	if name == "" || !KeyringAvailable() {
		return nil
	}
	if down.Load() {
		return errors.New("keyring: the keyring did not answer earlier this run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.CommandContext(ctx, "secret-tool", "clear", "service", service, "account", name)
	case "darwin":
		cmd = exec.CommandContext(ctx, "security", "delete-generic-password", "-a", name, "-s", service)
	}
	cmd.WaitDelay = 250 * time.Millisecond
	err := cmd.Run()
	var exit *exec.ExitError
	if err == nil || (errors.As(err, &exit) && exit.ExitCode() == 44) { // 44 = errSecItemNotFound
		return nil
	}
	if runtime.GOOS == "linux" && errors.As(err, &exit) && exit.ExitCode() == 1 && ctx.Err() == nil {
		if _, found := keyringGet(name); !found { // secret-tool clear exits 1 for an absent item too: nothing to delete
			return nil
		}
	}
	if ctx.Err() != nil {
		down.Store(true)
	}
	return fmt.Errorf("keyring: delete %s: %w", name, err)
}

// quote wraps s for the `security -i` tokenizer: double quotes, with backslash and quote escaped.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
