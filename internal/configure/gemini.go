// gemini.go — the gemini agent's submenu: Google login via `agy` or an AI Studio key, then Login and Mint key. Mint key
// creates a Gemini-only key through the Google Cloud API Keys API (internal/googlekeys) and rotates GEMINI_API_KEY to it.
package configure

import (
	"context"
	"errors"
	"os/exec"
	"time"

	"github.com/edotau/claude-code/internal/googlekeys"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vendors"
)

const (
	geminiRow    = "gemini"
	geminiKeyEnv = "GEMINI_API_KEY"
)

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
	key := keyRow(d, geminiRow, geminiRow, "pay per token · paste to set or rotate · every agent and ask leg")
	ensure := key.run
	key.run = func() bool {
		ok := ensure()
		if ok && loginSel {
			d.printf("gemini     the Google login via `agy` still takes priority over %s\n", geminiKeyEnv)
		}
		return ok
	}
	return agentMenu{st, []billingRow{
		{"Subscription", "Google login via agy · plan limits · interactive agy only " + bracket(loginSel, "logged in", "not logged in"), modeSubscription, func() bool {
			if loginSel {
				d.printf("%-10s on its Google login (kept)\n", geminiRow)
				return true
			}
			d.printf("gemini     signing in through `agy`; `ask --agent gemini` legs keep running on %s\n", geminiKeyEnv)
			return signIn(d, geminiRow, []string{"agy"})
		}},
		key,
		{"Login", "agy · browser sign-in on first launch, refreshes the Google session", "", func() bool {
			return signIn(d, geminiRow, []string{"agy"})
		}},
		{"Mint key", "API Keys API · mints a Gemini-only key in your Cloud project, rotates " + geminiKeyEnv, "", func() bool {
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
