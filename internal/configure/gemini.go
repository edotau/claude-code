package configure

import (
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/tui"
	"github.com/edotau/claude-code/internal/vendors"
)

const geminiRow = "gemini"

// geminiKeyStatus is the API key row's detail: where the key is stored and whether it still works.
func geminiKeyStatus(present bool, p *providers.Provider) string {
	if !present {
		return "no key"
	}
	status, _ := probe(keyTarget{provider: p})
	verdict := "probe ok"
	if status == "rejected" {
		verdict = "probe failed"
	}
	if _, src, ok := secrets.Get("GEMINI_API_KEY"); ok {
		return "stored in " + planeLabel(src) + ", " + verdict
	}
	return verdict
}

func planeLabel(src string) string {
	if src == "file" {
		return "secrets.env"
	}
	return src
}

func presence(ok bool) string {
	if ok {
		return "present"
	}
	return "absent"
}

// pickGeminiAuth asks which Gemini credential the session provider uses: a Gemini API key (every
// vendor and the harness ask runner can use it), or gemini's own Google login — which serves only the
// interactive gemini CLI's /auth and is never pinned, since the ask runner refuses a passthrough session.
func pickGeminiAuth(d *dialog, cur string) bool {
	p, hasProvider := d.reg.Providers[geminiRow]
	keyPresent := hasProvider && providers.CredentialPresent(p)
	loginPresent := vendors.GeminiLoginPresent()
	rows := [][2]string{
		{"API key", "pay per token · paste an AI Studio key to set or rotate [" + geminiKeyStatus(keyPresent, p) + "]"},
		{"Login", "gemini /auth · Google sign-in, refreshes the session · gemini CLI only [" + presence(loginPresent) + "]"},
	}
	start := 0
	if cur == geminiRow {
		start = 0
	}
	idx, err := tui.PickBack("gemini — pick the credential (current: "+cur+")", rows, start)
	if err != nil || idx < 0 {
		d.printf("provider   pinned %s (kept)\n", cur)
		return err == nil
	}
	if idx == 1 {
		d.printf("gemini   Google login serves the interactive gemini CLI only; the ask runner refuses a " +
			"passthrough gemini session, so it is never pinned as the session provider\n")
		if confirm(d, "Run `gemini` now to sign in via /auth?") {
			if err := runLogin([]string{"gemini"}); err != nil {
				d.printf("gemini: %v\n", err)
			}
		}
		return true
	}
	kt := keyTarget{label: geminiRow, envVar: "GEMINI_API_KEY", fixCmd: "claude-code secrets set GEMINI_API_KEY", provider: p}
	if !ensureKey(d, kt, keyPresent) {
		d.printf("gemini: no usable API key; left on %s\n", cur)
		return false
	}
	return keepOrPin(d, cur, geminiRow)
}
