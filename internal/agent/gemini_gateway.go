package agent

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
)

// AgyDefaultModel runs agy when the caller names no model: fast, and enough for headless legs.
const AgyDefaultModel = "gemini-3.8-flash-high"

// agyEfforts are the effort suffixes agy bakes into its model ids (gemini-3.8-flash-high).
var agyEfforts = []string{"low", "medium", "high", "xhigh", "max"}

// agyGatewayArg returns the dotted catalog id agy lists for model (effort suffix dropped) and the --model agy takes in
// gateway mode (base plus model's effort suffix); a non-Gemini id (a Databricks Claude slot) falls back to the
// GEMINI_MODEL pin, else AgyDefaultModel.
func agyGatewayArg(model string) (base, arg string) {
	base, effort, ok := agyGatewayBase(model)
	if !ok {
		base, effort, _ = agyGatewayBase(cmp.Or(providers.Pin(providers.PinGeminiModel), AgyDefaultModel))
	}
	if effort == "" {
		return base, base
	}
	return base, base + "-" + effort
}

// agyGatewayBase resolves id (bare, dotted, Databricks-spelled, or effort-suffixed) to a Gemini catalog id.
func agyGatewayBase(id string) (base, effort string, ok bool) {
	if before, suffix, cut := strings.CutLast(id, "-"); cut && slices.Contains(agyEfforts, suffix) {
		if b, _, ok := agyGatewayBase(before); ok {
			return b, suffix, true
		}
	}
	canonical, _, ok := models.Resolve(id)
	if !ok || models.FamilyKind(canonical) != models.FamilyGemini {
		return "", "", false
	}
	return models.Wire("dotted", canonical), "", true
}

// geminiGateway runs agy in its env-only gateway mode against the router's /p/<profile>/gemini route, which swaps
// in a fresh Databricks token per request; the key agy holds is a session token scoped to this profile.
func geminiGateway(ctx context.Context, p *providers.Provider, model string, h *Headless) ([]string, []string, error) {
	rb, err := ensureRouter(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("gemini: router: %w", err)
	}
	tok, err := mintSession(ctx, p.Name, os.Getpid())
	if err != nil {
		return nil, nil, fmt.Errorf("gemini: session token for profile %s: %w", p.Name, err)
	}
	base, arg := agyGatewayArg(model)
	env := []string{
		"AGY_LLM_GATEWAY_URL=" + proxy.SessionBase(rb, p.Name) + "/" + providers.ProtocolGemini,
		"AGY_LLM_GATEWAY_API_KEY=" + tok,
		"AGY_LLM_GATEWAY_WIRE_PROTOCOL=genai",
		"AGY_LLM_GATEWAY_MODELS=" + base,
		// every tier on the one served model: agy's side calls (titles) use the flash-lite tier
		"AGY_LLM_GATEWAY_TIER_FLASH=" + base, "AGY_LLM_GATEWAY_TIER_PRO=" + base, "AGY_LLM_GATEWAY_TIER_FLASH_LITE=" + base,
		// a caller's own gateway headers, CA bundle or proxy must not reach the loopback router
		"AGY_LLM_GATEWAY_HEADERS=", "AGY_LLM_GATEWAY_CA_CERT=", "AGY_LLM_GATEWAY_PROXY_URL=",
	}
	if h != nil {
		return env, nil, nil // headless argv is agent.RunAgy's (prompt over stdin stream-json)
	}
	return env, []string{"--model", arg}, nil
}
