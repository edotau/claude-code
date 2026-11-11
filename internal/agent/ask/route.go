package ask

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// ApplyRoute exports --route as HARNESS_ROUTE (databricks:<p> also as CLAUDE_PROFILE) for this process and its jobs.
func ApplyRoute(route, profile string) error {
	if route == "" {
		return nil
	}
	mode, p, err := router.ParseRoute(route)
	if err != nil {
		return err
	}
	if mode == router.RouteDatabricks {
		return applyArm(p, profile)
	}
	if arm := router.ArmProfile(); arm != "" {
		return errLeaveArm(arm)
	}
	return os.Setenv(router.EnvRoute, mode)
}

// applyArm resolves route databricks:<p> (bare: from profile, else CLAUDE_PROFILE), checks the row and exports it.
func applyArm(p, profile string) error {
	if p == "" {
		if p = bareArm(profile); p == "" {
			return errors.New("--route databricks needs a workspace: databricks:<profile>")
		}
	}
	if arm := router.ArmProfile(); arm != "" && p != arm {
		return errLeaveArm(arm)
	}
	if err := providers.ServingRow(p); err != nil {
		return err
	}
	if err := os.Setenv(router.EnvRoute, router.RouteDatabricks+":"+p); err != nil {
		return err
	}
	return os.Setenv(providers.EnvSessionProfile, p)
}

// errLeaveArm refuses a typed --route that differs from the inherited databricks:<arm>.
func errLeaveArm(arm string) error {
	return fmt.Errorf("--route: a nested dispatch cannot leave databricks:%s", arm)
}

// bareArm is the first of profile and CLAUDE_PROFILE that names a Databricks row, else "".
func bareArm(profile string) string {
	for _, name := range []string{profile, os.Getenv(providers.EnvSessionProfile)} {
		if name != "" && providers.ServingRow(name) == nil {
			return name
		}
	}
	return ""
}

// CheckArm refuses a flag that would take the dispatch off route databricks:<p>; nil outside the route.
func CheckArm(agentName, profile, provider, model string, direct bool) error {
	arm := router.ArmProfile()
	if arm == "" {
		return nil
	}
	if err := armFlags(arm, profile, provider, direct); err != nil {
		return err
	}
	reg, err := providers.Load()
	if err != nil {
		return fmt.Errorf("--route databricks:%s: %w", arm, err)
	}
	if prof, _, typed := strings.Cut(model, ":"); typed && reg.Providers[prof] != nil && prof != arm {
		return fmt.Errorf("--model %s: outside --route databricks:%s", model, arm)
	}
	return armModel(reg.Providers[arm], agentName, strings.TrimPrefix(model, arm+":"))
}

// armFlags refuses --direct, a --profile other than the workspace and a --provider other than databricks.
func armFlags(arm, profile, provider string, direct bool) error {
	switch {
	case direct:
		return fmt.Errorf("--direct: refused inside --route databricks:%s", arm)
	case profile != "" && profile != arm && profile != agent.LeadSession:
		return fmt.Errorf("--profile %s: outside --route databricks:%s", profile, arm)
	case provider != "" && provider != router.RouteDatabricks:
		return fmt.Errorf("--provider %s: outside --route databricks:%s", provider, arm)
	}
	return nil
}

// armModel refuses an explicit model id the workspace does not serve agentName; aliases and discovery pass.
func armModel(p *providers.Provider, agentName, model string) error {
	if model == "" || agentName == "" || model == router.DiscoveryModel || slices.Contains(router.Aliases, model) {
		return nil
	}
	served := providers.SpellPin(p, model) != ""
	if agentName != "claude" { // Serves covers external agents only
		_, why := agent.ProfileServes(agentName, p, model)
		served = why == ""
	}
	if !served {
		return fmt.Errorf("--model %s: not served to %s on databricks:%s", model, agentName, p.Name)
	}
	return nil
}
