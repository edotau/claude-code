// gemini.go — the gemini agent's submenu: Google login via `agy` or an AI Studio key, then Login and Mint key. Mint key
// creates a Gemini-only key through the Google Cloud API Keys API (internal/googlekeys) and rotates GEMINI_API_KEY to it.
package configure

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/googlekeys"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vendors"
)

const (
	geminiRow    = "gemini"
	geminiKeyEnv = "GEMINI_API_KEY"
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
	loginSel := vendors.GeminiLoginPresent() // agy's cached Google login beats GEMINI_API_KEY
	p, keyOK := keyPresent(d.reg, geminiRow)
	st := agentState{mode: modeNone}
	switch {
	case loginSel:
		st = agentState{mode: modeSubscription, verified: true}
	case keyOK:
		st = agentState{mode: modeAPIKey, verified: keyVerified(p)}
	}
	key := keyRow(d, geminiRow, geminiRow, "pay per token · paste to set or rotate · every ask leg")
	ensure := key.run
	key.run = func() bool {
		ok := ensure()
		if ok && loginSel {
			d.printf("agy        the Google login still takes priority over %s\n", geminiKeyEnv)
		}
		return ok
	}
	return agentMenu{st, []billingRow{
		{"Subscription", "Google login via agy · plan limits " + bracket(loginSel, "logged in", "not logged in"), modeSubscription, func() bool {
			if loginSel {
				d.printf("%-10s on its Google login (kept)\n", "agy")
				return true
			}
			d.printf("agy        signing in; `ask --agent gemini` legs keep running on %s\n", geminiKeyEnv)
			return runAgy(d)
		}},
		key,
		{"Login", "agy · browser sign-in, refreshes the Google session", "", func() bool {
			return runAgy(d)
		}},
		{"Mint key", "Cloud API Keys · mints a Gemini-only key, rotates " + geminiKeyEnv, "", func() bool {
			return mintGeminiKey(d)
		}},
	}}
}

// newKeysClient is the API Keys client seam; tests replace it.
var newKeysClient = googlekeys.New

// mintGeminiKey creates a key restricted to the Gemini API, stores it in the plane the old one came from, then offers
// to delete the old key. No Google Cloud login yet: offers `gcloud auth application-default login` first.
func mintGeminiKey(d *dialog) bool {
	forceFile, plane := planeOf(geminiKeyEnv)
	if plane == "env" {
		d.printf("%s: set via the process environment, not a stored plane — edit it where it is exported\n", geminiKeyEnv)
		return false
	}
	if !actionConfirmed("Mint a new Gemini-only API key in your Google Cloud project") {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, err := newKeysClient(ctx)
	if errors.Is(err, googlekeys.ErrNoADC) {
		if _, lookErr := exec.LookPath("gcloud"); lookErr != nil {
			d.printf("gemini: %v; install the Google Cloud CLI, or use API key to paste one from aistudio.google.com/apikey\n", err)
			return false
		}
		if !signIn(d, "gcloud", googlekeys.LoginArgv) {
			return false
		}
		c, err = newKeysClient(ctx)
	}
	if err != nil {
		d.printf("gemini: %v\n", err)
		return false
	}
	oldName := ""
	if old, _, ok := secrets.Get(geminiKeyEnv); ok && old != "" {
		oldName, _ = c.Lookup(ctx, old) // best effort: a key from another project or AI Studio may not resolve
	}
	name, key, err := c.Create(ctx, "claude-code gemini "+time.Now().Format("2006-01-02"))
	if err != nil {
		d.printf("gemini: %v\n", err)
		return false
	}
	d.printf("gemini     minted %s (new keys can take a minute to start working)\n", name)
	if !storeValue(d, geminiKeyEnv, key, forceFile) {
		return false
	}
	if oldName != "" && oldName != name && actionConfirmed("Delete the old key "+oldName+" (restorable for 30 days)") {
		if err := c.Delete(ctx, oldName); err != nil {
			d.printf("gemini: old key kept: %v\n", err)
		} else {
			d.printf("gemini     deleted %s\n", oldName)
		}
	}
	return true
}
