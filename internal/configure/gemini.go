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

// pickGeminiAuth asks which Gemini credential the session provider uses: gemini's own Google AI Pro
// login (preferred — serves the interactive gemini CLI and `ask --agent gemini` the same way codex's
// ChatGPT login does, but is never pinned as the Claude session's own provider, which refuses a
// passthrough session), or a Gemini API key as the fallback (every vendor and the ask runner can use it).
func pickGeminiAuth(d *dialog, cur string) bool {
	p, hasProvider := d.reg.Providers[geminiRow]
	keyPresent := hasProvider && providers.CredentialPresent(p)
	loginPresent := vendors.GeminiLoginPresent()
	rows := [][2]string{
		{"Login", "Google AI Pro login · gemini /auth → \"Login with Google\" [" + presence(loginPresent) + "]"},
		{"API key", "pay per token · fallback · paste an AI Studio key to set or rotate [" + geminiKeyStatus(keyPresent, p) + "]"},
	}
	idx, err := tui.PickBack("gemini — pick the credential (current: "+cur+")", rows, 0)
	if err != nil || idx < 0 {
		d.printf("provider   pinned %s (kept)\n", cur)
		return err == nil
	}
	if idx == 0 {
		d.printf("gemini   Google AI Pro login serves the interactive gemini CLI and `ask --agent gemini`; the " +
			"Claude session itself refuses a passthrough gemini provider, so it is never pinned here\n")
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
