package cli

import (
	"context"

	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/providers"
	usagecmd "github.com/edotau/claude-code/internal/usage"
)

// usageBudgets reads each Databricks profile's budgets for `usage --cost`; any failure is skipped quietly.
func usageBudgets() []usagecmd.Budget {
	reg, err := providers.Load()
	if err != nil {
		return nil
	}
	var out []usagecmd.Budget
	for _, profile := range providers.DatabricksProfiles(reg) {
		spends, err := databricks.ReadBudgetSpend(context.Background(), reg.Row(profile))
		if err != nil {
			continue
		}
		for _, s := range spends {
			out = append(out, usagecmd.Budget{Profile: profile, Number: s.Budget, Scope: s.Scope, Period: s.Period, Spend: s.Spend, Limit: s.Limit})
		}
	}
	return out
}
