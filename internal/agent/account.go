package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
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
		if hub.OwnPlan(p.OpencodeID) {
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

// hubDispatchFor maps an opencode cell "<opencodeID>/<model>" to the hub dispatch on that provider and the bare model.
func hubDispatchFor(reg *providers.Registry, cell string) (Engine, string, error) {
	id, model, ok := strings.Cut(cell, "/")
	if !ok || reg == nil {
		return nil, "", fmt.Errorf("cell %q is not <opencodeID>/<model>", cell)
	}
	var first *providers.Provider
	for _, name := range reg.Names() {
		p := reg.Providers[name]
		if p.OpencodeID != id {
			continue
		}
		if providers.CredentialPresent(p) { // rows can share an opencodeID (chatgpt, openai): the signed-in one serves it
			return hub.Dispatch("opencode", p, reg), model, nil
		}
		if first == nil {
			first = p
		}
	}
	if first != nil {
		return hub.Dispatch("opencode", first, reg), model, nil
	}
	// opencode's own plans (opencode-go, opencode) have no registry provider: opencode resolves them itself.
	if hub.OwnPlan(id) {
		return hub.Dispatch("opencode", &providers.Provider{Name: id, OpencodeID: id}, reg), model, nil
	}
	return nil, "", fmt.Errorf("no provider has opencodeID %q", id)
}

// The shared run-and-classify helpers for a vendor CLI's sign-in and version probes.

// ProbeCLI runs bin args on spec's environment; combined folds stderr into the returned output.
func ProbeCLI(ctx context.Context, bin string, spec EnvSpec, combined bool, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = spec.Environ(ctx)
	if combined {
		return cmd.CombinedOutput()
	}
	return cmd.Output()
}

// ClassifyProbe maps a failed probe: a deadline is transient, an exit the CLI chose is NeedsLogin, else transient.
func ClassifyProbe(ctx context.Context, err error) (providers.AuthVerdict, string) {
	if ctx.Err() != nil {
		return providers.VerdictTransient, ctx.Err().Error()
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok && ee.ExitCode() >= 0 {
		return providers.VerdictNeedsLogin, ""
	}
	return providers.VerdictTransient, ProbeErrDetail(err)
}

// probeDetailMax caps the stderr line ProbeErrDetail appends.
const probeDetailMax = 200

// ProbeErrDetail is err plus the first stderr line (≤200 bytes), when the command reported one.
func ProbeErrDetail(err error) string {
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		if line, _, _ := strings.Cut(strings.TrimSpace(string(ee.Stderr)), "\n"); line != "" {
			if len(line) > probeDetailMax {
				line = line[:probeDetailMax]
			}
			return err.Error() + ": " + line
		}
	}
	return err.Error()
}
