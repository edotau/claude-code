// gemini.go — the gemini agent's submenu: it runs on agy (Antigravity) with the Google sign-in only.
package configure

import (
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/vendors"
)

// agyCommand builds the agy process a sign-in launches; tests replace it with a fake binary.
var agyCommand = func() *exec.Cmd { return exec.Command("agy") }

// agySignInPoll is how often runAgy checks for agy's own login marker once it's running.
var agySignInPoll = 500 * time.Millisecond

// agySignInTimeout bounds how long runAgy waits for a sign-in before giving up and stopping agy.
var agySignInTimeout = 10 * time.Minute

// agyStopGrace is how long SIGINT gets to land before runAgy escalates to SIGTERM.
var agyStopGrace = 3 * time.Second

// runAgy starts agy on the real terminal, polls for its login marker every agySignInPoll, and stops it
// (SIGINT, then SIGTERM after agyStopGrace) the moment the marker appears — or on agySignInTimeout, or
// agy exits on its own. The caller never has to return to a menu with agy's own TUI still on screen.
func runAgy(d *dialog) bool {
	cmd := agyCommand()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		d.printf("agy: %v\n", err)
		return false
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	home, _ := os.UserHomeDir()
	marker := vendors.AgyLoginMarker(home)
	deadline := time.Now().Add(agySignInTimeout)
	ticker := time.NewTicker(agySignInPoll)
	defer ticker.Stop()
	for {
		if agySignedIn(marker) {
			stopAgy(cmd, done)
			d.printf("%-10s signed in\n", "agy")
			return true
		}
		select {
		case <-done:
			if agySignedIn(marker) { // agy may have written the marker right before exiting
				d.printf("%-10s signed in\n", "agy")
				return true
			}
			d.printf("agy: not signed in\n")
			return false
		case now := <-ticker.C:
			if now.After(deadline) {
				stopAgy(cmd, done)
				d.printf("agy: not signed in (timed out)\n")
				return false
			}
		}
	}
}

// agySignedIn reports whether agy's login marker is on disk and non-empty (fully written, not mid-write).
func agySignedIn(marker string) bool {
	fi, err := os.Stat(marker)
	return err == nil && fi.Size() > 0
}

// stopAgy asks a still-running agy to exit: SIGINT, SIGTERM after agyStopGrace if that didn't land.
func stopAgy(cmd *exec.Cmd, done <-chan error) {
	_ = cmd.Process.Signal(syscall.SIGINT)
	select {
	case <-done:
	case <-time.After(agyStopGrace):
		_ = cmd.Process.Signal(syscall.SIGTERM)
		<-done
	}
}

func geminiMenu(d *dialog) agentMenu {
	loginSel := vendors.GeminiLoginPresent()
	st := agentState{mode: modeNone}
	if loginSel {
		st = agentState{mode: modeSubscription, verified: true}
	}
	return agentMenu{st, []billingRow{
		{"Subscription", "Google login via agy · plan limits " + bracket(loginSel, "logged in", "not logged in"), modeSubscription, func() bool {
			if loginSel {
				d.printf("%-10s on its Google login (kept)\n", "agy")
				return true
			}
			return runAgy(d)
		}},
		{"Login", "agy · browser sign-in, refreshes the Google session", "", func() bool {
			return runAgy(d)
		}},
	}}
}
