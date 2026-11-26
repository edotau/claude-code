package gemini

import (
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/driver/googletools"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// agyGatewayHeader is the gateway headers value: the google-tools header for a headless run that asked, else "".
func agyGatewayHeader(p *providers.Provider, base string, h *agent.Headless) (string, error) {
	if h == nil || len(h.GoogleTools) == 0 {
		return "", nil
	}
	return agyGoogleToolsHeader(p, base, h.GoogleTools)
}

// agyGoogleToolsHeader is the proxy header for tools; preflight refuses a foreign model or an unserved tool.
func agyGoogleToolsHeader(p *providers.Provider, base string, names []string) (string, error) {
	asked, err := googletools.Parse(names)
	if err != nil {
		return "", err
	}
	if agyForeign(base) {
		return "", fmt.Errorf("gemini: --google-tools needs a Gemini model; %s is not one", base)
	}
	if served := p.Options.GoogleTools; served != nil {
		ok, _ := googletools.Parse(served)
		if miss := googletools.Unserved(ok, asked); len(miss) > 0 {
			return "", &agent.PreflightError{Finding: agent.Finding{Check: "google-tools", Profile: p.Name, Model: base,
				Detail: fmt.Sprintf("%s does not serve google tool %s; served: %s", p.Name,
					googletools.Join(miss), strings.Join(served, ","))}}
		}
	} else {
		fmt.Fprintf(os.Stderr, "gemini: google tools unprobed on %s; sent as asked\n", p.Name)
	}
	// the model token scopes the tools to the main model; agy's flash-lite title calls reach the same gateway
	return googletools.Header + ": " + googletools.Join(asked) + "," +
		googletools.ModelPrefix + base, nil
}

// agySettingsKey maps a tool to its agy --settings key; Phase 0.1 found none, so every tool is refused.
var agySettingsKey = map[googletools.Tool]string{}

// agyLoginGoogleTools refuses a tool agy's own Google login has no setting for.
func agyLoginGoogleTools(p *providers.Provider, model string, names []string) error {
	asked, err := googletools.Parse(names)
	if err != nil {
		return err
	}
	profile := ""
	if p != nil {
		profile = p.Name
	}
	for _, t := range asked {
		if _, ok := agySettingsKey[t]; !ok {
			return &agent.PreflightError{Finding: agent.Finding{Check: "google-tools", Profile: profile, Model: model,
				Detail: "agy's own login has no setting for google tool " + string(t)}}
		}
	}
	return nil
}
