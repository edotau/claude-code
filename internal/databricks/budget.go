package databricks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

type BudgetSpend struct {
	Budget        int
	Scope, Period string
	Spend, Limit  float64
}

// ReadBudgetSpend reads current workspace caps without retaining account or budget resource identifiers.
func ReadBudgetSpend(ctx context.Context, p *providers.Provider) ([]BudgetSpend, error) {
	if p == nil || providers.PresetHost(p.Host) == "" {
		return nil, errors.New("budget workspace unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ctx = providers.WithNotes(ctx, func(string) {})
	status, body, err := apiGet(ctx, p, providers.PresetHost(p.Host), "/api/ai-gateway/v2/budget-spend-statuses")
	if err != nil || status < http.StatusOK || status >= http.StatusMultipleChoices {
		return nil, errors.New("budget spend unavailable")
	}
	return decodeBudgetSpend(body)
}

func decodeBudgetSpend(body []byte) ([]BudgetSpend, error) {
	var reply struct {
		Budgets []struct {
			Alerts []struct {
				Scope     string      `json:"scope_type"`
				Period    string      `json:"time_period"`
				Spend     json.Number `json:"spend"`
				Threshold json.Number `json:"threshold"`
			} `json:"alert_spend_statuses"`
		} `json:"budget_spend_statuses"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return nil, errors.New("invalid budget response")
	}
	var out []BudgetSpend
	for i, budget := range reply.Budgets {
		for _, alert := range budget.Alerts {
			spend, errSpend := alert.Spend.Float64()
			limit, errLimit := alert.Threshold.Float64()
			if errSpend != nil || errLimit != nil || !finiteBudget(spend) || !finiteBudget(limit) || spend < 0 || limit <= 0 {
				return nil, fmt.Errorf("invalid spend or limit for budget %d", i+1)
			}
			out = append(out, BudgetSpend{Budget: i + 1, Scope: alert.Scope, Period: alert.Period, Spend: spend, Limit: limit})
		}
	}
	return out, nil
}

func finiteBudget(n float64) bool {
	return !math.IsNaN(n) && !math.IsInf(n, 0)
}
