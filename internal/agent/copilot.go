package agent

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/agent/copilotreg"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// copilot BYOK: COPILOT_PROVIDER_* in the env; COPILOT_OFFLINE keeps it off GitHub's own model path.
// copilotLogin runs copilot on its GitHub sign-in (`copilot login`, or GH_TOKEN); blank BYOK vars cannot redirect it.
func copilotLogin(model string, h *Headless) ([]string, []string, error) {
	if model == "auto" { // no pin: blank (copilot reads "" as unset) so copilot's own /model default applies
		model = ""
	}
	env := []string{"COPILOT_MODEL=" + model, "COPILOT_PROVIDER_BASE_URL=", "COPILOT_OFFLINE=", "COPILOT_PROVIDERS_CONFIG="}
	tok, err := CopilotLaunchToken()
	if err != nil {
		return nil, nil, err
	}
	if tok != "" { // the harness's chosen account beats any inherited token
		env = append(env, CopilotTokenEnv+"="+tok)
	}
	if h == nil {
		return env, copilotPinArgv(), nil
	}
	return env, copilotHeadless(h), nil
}

// copilotPinArgv is --reasoning-effort/--context from the configure page's pins, on every interactive launch.
func copilotPinArgv() []string {
	var argv []string
	if e := providers.Pin(providers.PinCopilotEffort); e != "" {
		argv = append(argv, "--reasoning-effort", e)
	}
	if c := providers.Pin(providers.PinCopilotContext); c != "" {
		argv = append(argv, "--context", c)
	}
	return argv
}

// copilotWire wires BYOK; window is the model's context window (providers.ContextWindow), which sizes the prompt cap.
func copilotWire(
	p *providers.Provider, dialect, base, model string, window int, cred string, routed bool, h *Headless,
) ([]string, []string, error) {
	typ := "openai"
	if dialect == providers.ProtocolAnthropic {
		typ = "anthropic"
	}
	wire, modelID := "", ""
	if routed && dialect == providers.ProtocolAnthropic && models.FamilyKind(model) != models.FamilyClaude {
		w, canonical, ok, why := copilotreg.EndpointWire(p, model)
		if !ok {
			return nil, nil, fmt.Errorf("copilot: %s", why)
		}
		if w != providers.ProtocolAnthropic { // the router's openai|responses passthrough
			wire, modelID = w, canonical
			typ, base = "openai", base+"/"+wire
		}
	}
	window, output := copilotreg.Caps(window, model)
	env := []string{
		"COPILOT_PROVIDER_BASE_URL=" + base,
		"COPILOT_PROVIDER_TYPE=" + typ,
		"COPILOT_MODEL=" + model,
		"COPILOT_PROVIDER_MAX_OUTPUT_TOKENS=" + strconv.Itoa(output),
		"COPILOT_OFFLINE=true",
		// Blank every BYOK credential var so a token exported in the parent shell cannot outrank this route.
		"COPILOT_PROVIDER_API_KEY=", "COPILOT_PROVIDER_BEARER_TOKEN=", "COPILOT_PROVIDER_API_KEY_COMMAND=",
	}
	if wire == providers.ProtocolResponses {
		env = append(env, "COPILOT_PROVIDER_WIRE_API=responses")
	}
	if modelID != "" {
		env = append(env, "COPILOT_PROVIDER_MODEL_ID="+modelID)
	}
	// Prompt budget = window minus the output cap and a 10% margin, so copilot compacts before the endpoint refuses.
	if n := copilotreg.PromptTokens(window, output); n > 0 {
		env = append(env, "COPILOT_PROVIDER_MAX_PROMPT_TOKENS="+strconv.Itoa(n))
	}
	if cred != "" {
		// Bearer precedence is copilot's own rule; sending the key on both headers trips strict gateways.
		if p.Auth.HeaderStyle() == providers.HeaderBearer {
			env = append(env, "COPILOT_PROVIDER_BEARER_TOKEN="+cred)
		} else {
			env = append(env, "COPILOT_PROVIDER_API_KEY="+cred)
		}
	}
	if len(p.Headers) > 0 {
		var hs []string
		for _, k := range slices.Sorted(maps.Keys(p.Headers)) {
			hs = append(hs, k+": "+p.Headers[k])
		}
		env = append(env, "COPILOT_PROVIDER_HEADERS="+strings.Join(hs, "\n"))
	}
	if h == nil {
		return env, copilotPinArgv(), nil
	}
	return env, copilotHeadless(h), nil
}

func copilotHeadless(h *Headless) []string {
	argv := []string{"--prompt", h.Prompt}
	if h.Tools {
		argv = append(argv, "--allow-all-tools")
	}
	return argv
}

// CopilotLoginPresent reports a usable GitHub login for copilot: a token env var, the keyring token, its own
// stored login, or gh's active account.
func CopilotLoginPresent() bool {
	_, ok := copilotAuthSource()
	return ok
}
