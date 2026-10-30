// gemini.go — the gemini agent's submenu: it runs on agy (Antigravity) with the Google sign-in only.
package configure

import (
	"bytes"
	"os"
	"os/exec"
	"slices"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/tui"
	"github.com/edotau/claude-code/internal/agent/vendors"
)

// agyCommand builds the agy process a sign-in launches; tests replace it with a fake binary.
var agyCommand = func() *exec.Cmd {
	bin, err := paths.VendorBin("agy")
	if err != nil {
		return &exec.Cmd{Err: err}
	}
	return exec.Command(bin)
}

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
	defer tui.Preserve()()
	home, _ := os.UserHomeDir()
	marker := vendors.AgyLoginMarker(home)
	before, _ := os.ReadFile(marker)
	beforeInfo, _ := os.Stat(marker)
	cmd := agyCommand()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		d.printf("agy: %v\n", err)
		return false
	}
	done := make(chan struct{}) // closed on exit; the login marker, not agy's exit status, decides sign-in
	go func() { _ = cmd.Wait(); close(done) }()
	deadline := time.Now().Add(agySignInTimeout)
	ticker := time.NewTicker(agySignInPoll)
	defer ticker.Stop()
	for {
		if agySignedInSince(marker, before, beforeInfo) {
			stopAgy(cmd, done)
			d.printf("%-10s signed in\n", "agy")
			return true
		}
		select {
		case <-done:
			if agySignedInSince(marker, before, beforeInfo) { // agy may have written the marker right before exiting
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

func agySignedInSince(marker string, before []byte, beforeInfo os.FileInfo) bool {
	after, err := os.ReadFile(marker)
	if err != nil || len(after) == 0 {
		return false
	}
	if !bytes.Equal(after, before) {
		return true
	}
	afterInfo, err := os.Stat(marker)
	return err == nil && (beforeInfo == nil || !afterInfo.ModTime().Equal(beforeInfo.ModTime()))
}

// stopAgy asks a still-running agy and its descendants to exit: SIGINT, SIGTERM after agyStopGrace.
func stopAgy(cmd *exec.Cmd, done <-chan struct{}) {
	desc := proc.Descendants(cmd.Process.Pid)
	_ = cmd.Process.Signal(syscall.SIGINT)
	signalDescendants(desc, syscall.SIGINT)
	select {
	case <-done:
		waitOrTermDescendants(desc)
	case <-time.After(agyStopGrace):
		_ = cmd.Process.Signal(syscall.SIGTERM)
		signalAliveDescendants(desc, syscall.SIGTERM)
		<-done
	}
}

func waitOrTermDescendants(pids []int) {
	deadline := time.Now().Add(agyStopGrace)
	for {
		alive := slices.ContainsFunc(pids, proc.Alive)
		if !alive {
			return
		}
		if time.Now().After(deadline) {
			signalAliveDescendants(pids, syscall.SIGTERM)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func signalDescendants(pids []int, sig syscall.Signal) {
	self := os.Getpid()
	for _, pid := range pids {
		if pid <= 1 || pid == self {
			continue
		}
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Signal(sig)
		}
	}
}

func signalAliveDescendants(pids []int, sig syscall.Signal) {
	var alive []int
	for _, pid := range pids {
		if proc.Alive(pid) {
			alive = append(alive, pid)
		}
	}
	signalDescendants(alive, sig)
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
