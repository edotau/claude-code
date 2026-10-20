// Package keyring is the OS secret-service plane: a shell-out to `secret-tool` (libsecret) on Linux,
// `security` on macOS. Degrades rather than failing hard: no tool, no session, or a locked keyring
// all read as "this plane holds nothing", so callers fall through to the file plane.
package keyring

import (
	"context"
	"errors"
	"fmt"
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
// names (`secrets status`) pays the timeout once, not once per name.
var down atomic.Bool

// ErrNoAnswer means the tool timed out or the `down` latch was already set — not a plain miss.
var ErrNoAnswer = errors.New("keyring: did not answer")

// Tool returns the plane's binary name for this OS, or "" where none is wired up.
func Tool() string {
	switch runtime.GOOS {
	case "linux":
		return "secret-tool"
	case "darwin":
		return "security"
	default:
		return ""
	}
}

// Available reports whether a keyring tool is on PATH for this OS.
func Available() bool {
	b := Tool()
	if b == "" {
		return false
	}
	_, err := exec.LookPath(b)
	return err == nil
}

// Lookup looks up name in the OS keyring. err is ErrNoAnswer when the tool timed out or the
// `down` latch is already set; a plain miss, unsupported OS, or no tool all read as ("", false, nil).
func Lookup(name string) (value string, found bool, err error) {
	if name == "" || !Available() {
		return "", false, nil
	}
	if down.Load() {
		return "", false, ErrNoAnswer
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
		return "", false, ErrNoAnswer
	}
	if err2 != nil {
		return "", false, nil // plain miss (account not stored)
	}
	v := strings.TrimSpace(string(out))
	return v, v != "", nil
}

// Get is Lookup's "found?" shape, the API most callers want: ErrNoAnswer reads as a miss too.
func Get(name string) (string, bool) {
	v, found, _ := Lookup(name)
	return v, found
}

// Set stores value under name, overwriting any existing entry. The value always goes on stdin, never
// argv: an argv secret is visible to every same-uid process (`ps`) for the life of the call.
func Set(name, value string) error {
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
	if !Available() {
		return fmt.Errorf("keyring: no keyring tool on PATH for %s — use the file plane instead", runtime.GOOS)
	}
	if down.Load() {
		return errors.New("keyring: the keyring did not answer earlier this run — use the file plane instead")
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

// Delete removes name from the keyring; a name that is not stored is not an error, a refused delete is.
func Delete(name string) error {
	if name == "" || !Available() {
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
		if _, found := Get(name); !found { // secret-tool clear exits 1 for an absent item too: nothing to delete
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
