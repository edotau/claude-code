package agent

import (
	"cmp"
	"fmt"
	"path/filepath"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// RouteDetail returns the "<agent> route" detail string DoctorChecks would emit for the agent, without
// running the rest of the doctor sweep; ok is false for an unknown agent or a registry load error.
func RouteDetail(name string) (detail string, ok bool) {
	return schemaFor(name).eng().routeDetail()
}

// loadSelection is the registry and its unpinned selection; ok is false when the registry does not load.
func loadSelection() (reg *providers.Registry, sel providers.Selection, selErr error, ok bool) {
	reg, err := providers.Load()
	if err != nil || reg == nil {
		return nil, providers.Selection{}, nil, false
	}
	sel, selErr = providers.Select(reg, "")
	return reg, sel, selErr, true
}

// opencodeRouteDetail reports the opencode route `claude-code configure --restore` would render: provider,
// model/small_model (the github-copilot route only), the file it writes, and — names/booleans only,
// never a credential — whether opencode has its own GitHub Copilot login and whether the copilot CLI has one.
func opencodeRouteDetail(reg *providers.Registry, sel providers.Selection, selErr error) string {
	own := OpencodeOwnLoginPresent()
	opencodeLogin := own || OpencodeCopilotLoginPresent()
	copilotCLILogin := CopilotLoginPresent()
	if own { // opencode's own Go/Zen credential serves the routed opencode dispatches, whatever the session provider
		return fmt.Sprintf("provider=opencode-go dispatch=%s opencode_login=true copilot_cli_login=%v", RouteOwnLogin, copilotCLILogin)
	}
	if selErr != nil {
		return fmt.Sprintf("opencode_login=%v copilot_cli_login=%v (%v)", opencodeLogin, copilotCLILogin, selErr)
	}
	target, _, err := schemaFor("opencode").ResolveTarget(reg, sel, false)
	if err != nil || target.Provider == nil {
		return fmt.Sprintf("opencode_login=%v copilot_cli_login=%v (%v)", opencodeLogin, copilotCLILogin, errText(err))
	}
	file := filepath.Join(paths.StateDir(), "opencode", "opencode.json") // the overlay, not opencode's own config
	if target.Provider.Provider != "github" {
		return fmt.Sprintf("provider=%s file=%s opencode_login=%v copilot_cli_login=%v", target.Provider.Name, file, opencodeLogin, copilotCLILogin)
	}
	mainID, smallID, ok := OpencodeCopilotModelIDs(reg)
	if !ok {
		return fmt.Sprintf("provider=%s file=%s (no sonnet/haiku model configured) opencode_login=%v copilot_cli_login=%v", target.Provider.Name, file, opencodeLogin, copilotCLILogin)
	}
	// opus/sonnet/haiku here are github-copilot's own table (configure github), separate from COPILOT_MODEL.
	opusID := target.Provider.Models[models.Opus]
	return fmt.Sprintf("provider=%s model=github-copilot/%s small_model=github-copilot/%s opus/sonnet/haiku=%s/%s/%s file=%s opencode_login=%v copilot_cli_login=%v",
		target.Provider.Name, mainID, smallID, opusID, mainID, smallID, file, opencodeLogin, copilotCLILogin)
}

// copilotRouteDetail reports the copilot CLI's resolved dispatch (DescribeRoute): its own GitHub login,
// BYOK with an anthropic or gemini key, or none — names/booleans/dispatch only, never a credential. On the
// GitHub login dispatch it also names COPILOT_MODEL's source and the effort/context pins every launch reads.
func copilotRouteDetail(reg *providers.Registry, selErr error) string {
	if selErr != nil {
		return fmt.Sprintf("(%v)", selErr)
	}
	ri := schemaFor("copilot").DescribeRoute(reg)
	if !ri.OK {
		return ri.Reason
	}
	detail := fmt.Sprintf("dispatch=%s provider=%s model=%s (%s)", ri.Route, ri.Provider, ri.Model, ri.Reason)
	if ri.Route == RouteCopilotLogin {
		_, source := providers.CopilotModel(reg)
		rec := GetAgentRecord("copilot")
		effort, context := cmp.Or(rec.Effort, "default"), cmp.Or(rec.Context, "default")
		detail += fmt.Sprintf(" source=%s effort=%s context=%s", source, effort, context)
	}
	return detail
}

// codexRouteDetail reports codex's own CODEX_MODEL/CODEX_REASONING_EFFORT pins — names/booleans only,
// never a credential; unset means codex's own default (config.toml, then its ChatGPT-login catalog).
func codexRouteDetail() string {
	model, source := providers.CodexModel()
	effort, _ := providers.CodexEffort()
	return fmt.Sprintf("model=%s source=%s effort=%s", cmp.Or(model, "(default)"), source, cmp.Or(effort, "default"))
}

// geminiRouteDetail reports agy's own GEMINI_MODEL pin (effort rides the id's own suffix) — unset
// means agy's own default (AgyDefaultModel).
func geminiRouteDetail() string {
	model, source := providers.GeminiModel()
	return fmt.Sprintf("model=%s source=%s", cmp.Or(model, "(default)"), source)
}

// errText is err's message, or "" for nil.
func errText(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}
