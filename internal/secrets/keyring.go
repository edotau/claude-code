// Keyring plane: a shell-out to the OS secret service — `secret-tool` (libsecret) on Linux,
// `security` on macOS. Degrades rather than failing hard: no tool, no session, or a locked keyring
// all read as "this plane holds nothing", so callers fall through to the file plane.
package secrets

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// keyringService labels every account this harness stores (the service name for secret-tool, the
// -s argument for `security`).
const keyringService = "claude-code"

// keyringTimeout bounds one keyring call; a locked keyring can otherwise block on an unlock prompt
// with no agent to answer it.
var keyringTimeout = 3 * time.Second

// keyringDown latches once a lookup/store has already timed out this process, so a loop over many
// names (`secrets status`) pays the timeout once, not once per name.
var keyringDown bool

// keyringBin returns the plane's binary name for this OS, or "" where none is wired up.
func keyringBin() string {
	switch runtime.GOOS {
	case "linux":
		return "secret-tool"
	case "darwin":
		return "security"
	default:
		return ""
	}
}

// KeyringAvailable reports whether a keyring tool is on PATH for this OS.
func KeyringAvailable() bool {
	bin := keyringBin()
	if bin == "" {
		return false
	}
	_, err := exec.LookPath(bin)
	return err == nil
}

// KeyringGet looks up name in the OS keyring. A miss, an absent/unreachable tool, or an
// unsupported OS all read as (false), never an error — the caller's job is to fall through.
func KeyringGet(name string) (string, bool) {
	if name == "" || keyringDown || !KeyringAvailable() {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), keyringTimeout)
	defer cancel()
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.CommandContext(ctx, "secret-tool", "lookup", "service", keyringService, "account", name)
	case "darwin":
		cmd = exec.CommandContext(ctx, "security", "find-generic-password", "-a", name, "-s", keyringService, "-w")
	}
	cmd.WaitDelay = 250 * time.Millisecond
	out, err := cmd.Output()
	if ctx.Err() != nil {
		keyringDown = true
		return "", false
	}
	if err != nil {
		return "", false // plain miss (account not stored)
	}
	v := strings.TrimSpace(string(out))
	return v, v != ""
}

// KeyringSet stores value under name, overwriting any existing entry. The value goes on stdin
// (Linux) or argv (macOS, `security` has no stdin form) — callers must not log the invocation.
func KeyringSet(name, value string) error {
	if name == "" {
		return fmt.Errorf("secrets: empty name")
	}
	if value == "" {
		return fmt.Errorf("secrets: refusing to store an empty value for %s", name)
	}
	if !KeyringAvailable() {
		return fmt.Errorf("secrets: no keyring tool on PATH for %s — use the file plane instead", runtime.GOOS)
	}
	if keyringDown {
		return fmt.Errorf("secrets: the keyring did not answer earlier this run — use the file plane instead")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*keyringTimeout)
	defer cancel()
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.CommandContext(ctx, "secret-tool", "store", "--label="+keyringService,
			"service", keyringService, "account", name)
		cmd.Stdin = strings.NewReader(value)
	case "darwin":
		cmd = exec.CommandContext(ctx, "security", "add-generic-password", "-a", name, "-s", keyringService,
			"-w", value, "-U")
	}
	cmd.WaitDelay = 250 * time.Millisecond
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			keyringDown = true
		}
		return fmt.Errorf("secrets: keyring store %s: %w", name, err)
	}
	return nil
}
