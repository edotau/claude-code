package agent

import (
	"strings"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// ResolveAccount is resolveAccount for callers outside the package (the gateway view).
var ResolveAccount = resolveAccount

// resolveAccount is the account an agent's dispatch actually bills: "" keeps the table's key (the agent's own login
// or subscription); an agent resolving to another profile (an API key, a BYOK route) bills that <provider>·<profile>.
func resolveAccount(agentName string, c router.Cell) (string, string) {
	var t providers.Target
	switch agentName {
	case "opencode":
		return "", "" // the cell's provider prefix is the account; Configured gates its credential
	case "claude":
		_, ct, err := resolveTarget(RunOptions{Provider: providers.SessionProfile(), Model: c.Model})
		if err != nil || ct.Provider == nil || ct.Provider.Auth.Method() == providers.AuthCLI {
			return "", ""
		}
		t = ct
	default:
		if ownLoginAccount(agentName, c.Account) {
			return "", "" // the agent's own login cell keeps its account; a session profile never replaces it
		}
		t = billedAgentTarget(agentName, c.Model)
		if t.Provider == nil {
			return "", ""
		}
	}
	return t.Provider.Account(), router.DefaultBilling(t.Provider)
}

func billedAgentTarget(agentName, model string) providers.Target {
	t, own, _, err := SchemaFor(agentName).targetNote(RunOptions{Provider: sessionProvider(agentName), Model: model})
	if err != nil || own || t.Provider == nil || t.Provider.Auth.Login == agentName {
		return providers.Target{}
	}
	return t
}

// sessionProvider is the session's profile when it serves the agent, else "" (the vault pin and fallbacks decide).
func sessionProvider(agentName string) string {
	reg, err := providers.Load()
	if err != nil {
		return ""
	}
	p := reg.Providers[providers.SessionLead(reg, false)]
	if p != nil && (agentName == "claude" || CanServe(agentName, p)) {
		return p.Name
	}
	return ""
}

// ownLoginAccount is true when account is the <provider>·<profile> of a profile the agent's own CLI login backs.
func ownLoginAccount(agentName, account string) bool {
	_, profile, ok := strings.Cut(account, "·")
	if !ok {
		return false
	}
	reg, err := providers.Load()
	if err != nil {
		return false
	}
	p := reg.Providers[profile]
	return p != nil && p.Auth.Login == agentName && p.Account() == account
}

// explicitAccount is the <provider>·<profile> key of the named backend; a hub dispatch names its own profile.
func explicitAccount(r Engine, req RunOptions) (string, string) {
	if l, ok := r.(HubEngine); ok {
		p := l.HubProvider()
		if hub != nil && hub.OwnPlan(p.OpencodeID) {
			return "opencode·go", router.BillSubscription
		}
		return p.Account(), router.DefaultBilling(p)
	}
	reg, err := providers.Load()
	if err != nil {
		return "", ""
	}
	name := req.Provider
	if name == "" {
		name, _, _ = strings.Cut(req.Model, ":")
	}
	p, err := reg.Get(name)
	if err != nil {
		return "", ""
	}
	if p.Auth.Login == "copilot" { // the GitHub login bills the pinned profile
		return copilotLoginAccount(), router.DefaultBilling(p)
	}
	return p.Account(), router.DefaultBilling(p)
}

// copilotLoginAccount is the pinned copilot profile's route-state key (github·<profile>), or "" when no user is pinned.
func copilotLoginAccount() string {
	if u := router.CopilotUser(); u != "" {
		return "github·" + u
	}
	return ""
}
