package menu

import (
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/gemini"
)

// runAgy is AgySignIn with its message printed on d.
func runAgy(d *dialog) bool {
	ok, msg := configure.AgySignIn()
	d.printf("%s\n", msg)
	return ok
}

func geminiMenu(d *dialog) agentMenu {
	loginSel := gemini.GeminiLoginPresent()
	st := agentState{mode: modeNone}
	if loginSel {
		st = agentState{mode: modeSubscription, verified: true}
	}
	return agentMenu{st, []billingRow{
		{"Subscription", "Google login via agy · plan limits " + Bracket(loginSel, "logged in", "not logged in"), modeSubscription, func() bool {
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
