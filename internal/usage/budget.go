// budget.go — the session Databricks workspace's AI Gateway budget/spend statuses, cached between `usage` runs.

package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/network/httpx"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
)

const (
	budgetSchema   = 1
	budgetPageSize = 100
	budgetMaxPages = 20
)

// budgetCacheTTL is how long a fetched budget stays fresh before `usage` hits the network again.
const budgetCacheTTL = 5 * time.Minute

// budgetCache is state/harness/usage/gateway-budget.json: the rows `usage` fetched (empty when the endpoint is absent).
type budgetCache struct {
	Schema    int       `json:"schema"`
	FetchedAt time.Time `json:"fetched_at"`
	Profile   string    `json:"profile"`
	Rows      []quota   `json:"rows"`
}

var budgetCacheFormat = schemas.Register(schemas.Format{
	Name: "gateway-budget-cache", Owner: "internal/usage", Kind: schemas.Cache,
	Glob:    budgetCachePath,
	New:     func() any { return &budgetCache{} },
	Version: budgetSchema, Keep: "rebuildable cache; a version mismatch is a miss",
})

func budgetCachePath() string { return filepath.Join(paths.StateDir(), "usage", "gateway-budget.json") }

func readBudgetCache(path string) *budgetCache {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var c budgetCache
	if !schemas.DecodeOK(budgetCacheFormat, raw, &c) {
		return nil
	}
	return &c
}

func writeBudgetCache(path string, c budgetCache) {
	c.Schema = budgetSchema
	_ = paths.WriteJSON(path, c)
}

// sessionBudgetQuotas returns the session profile's budget rows when it is a Databricks workspace, else nil.
func sessionBudgetQuotas(ctx context.Context) []quota {
	reg, err := providers.Load()
	if err != nil {
		return nil
	}
	sel, err := providers.Select(reg, "")
	if err != nil || sel.Provider == nil || sel.Provider.Preset != providers.PresetDatabricks {
		return nil
	}
	rows := withTokenExpiry(budgetQuotas(ctx, sel.Provider, time.Now()), sel.Provider.Name)
	for i := range rows {
		rows[i].AuthMethod = sel.Provider.Auth.Method()
	}
	return rows
}

// withTokenExpiry stamps every row with the profile's recorded token expiry (kept out of the cache so it stays fresh).
func withTokenExpiry(rows []quota, profile string) []quota {
	exp, ok := providers.TokenExpiry(profile)
	if !ok {
		return rows
	}
	for i := range rows {
		rows[i].TokenExpiresAt = exp.UTC().Format(time.RFC3339)
	}
	return rows
}

// errBudgetAbsent is a 404 or 403: the workspace does not serve the budget endpoint to this caller.
var errBudgetAbsent = errors.New("budget endpoint unavailable")

// budgetQuotas serves the cache while fresh, else fetches; any failure yields a note row (404/403 cached, others not).
func budgetQuotas(ctx context.Context, p *providers.Provider, now time.Time) []quota {
	path := budgetCachePath()
	if c := readBudgetCache(path); c != nil && c.Profile == p.Name && now.Sub(c.FetchedAt) < budgetCacheTTL {
		return c.Rows
	}
	rows, err := fetchBudgetRows(ctx, p)
	if errors.Is(err, errBudgetAbsent) {
		rows = []quota{{Provider: "databricks", Account: p.Name, Note: "unavailable: " + err.Error()}}
		writeBudgetCache(path, budgetCache{FetchedAt: now, Profile: p.Name, Rows: rows})
		return rows
	}
	if err != nil {
		return []quota{{Provider: "databricks", Account: p.Name, Note: "unavailable: budget " + commandErrorLine(nil, err)}}
	}
	if slices.ContainsFunc(rows, isMalformedBudget) { // a bad row is re-fetched next run, not held for 5 minutes
		return rows
	}
	writeBudgetCache(path, budgetCache{FetchedAt: now, Profile: p.Name, Rows: rows})
	return rows
}

// isMalformedBudget reports whether q is a budget row that could not be parsed.
func isMalformedBudget(q quota) bool {
	return strings.HasPrefix(q.Note, budgetMalformedNote)
}

// budgetStatus is one entry of GET /api/ai-gateway/v2/budget-spend-statuses (undocumented); numbers are strings.
type budgetStatus struct {
	Name   string `json:"name"`
	Alerts []struct {
		Scope    string `json:"scope_type"`
		Period   string `json:"time_period"`
		Spend    string `json:"spend"`
		Limit    string `json:"threshold"`
		Consumed string `json:"percent_consumed"`
	} `json:"alert_spend_statuses"`
}

func fetchBudgetRows(ctx context.Context, p *providers.Provider) ([]quota, error) {
	host := providers.PresetHost(p.Host)
	if host == "" {
		return nil, errors.New("no workspace host")
	}
	var rows []quota
	token := ""
	for page := 1; page <= budgetMaxPages; page++ {
		u := host + "/api/ai-gateway/v2/budget-spend-statuses?page_size=" + strconv.Itoa(budgetPageSize)
		if token != "" {
			u += "&page_token=" + url.QueryEscape(token)
		}
		var doc struct {
			Statuses []budgetStatus `json:"budget_spend_statuses"`
			Next     string         `json:"next_page_token"`
		}
		if err := budgetGet(ctx, p, u, &doc); err != nil {
			return nil, err
		}
		for _, s := range doc.Statuses {
			rows = append(rows, budgetRows(p.Name, s)...)
		}
		if token = doc.Next; token == "" {
			break
		}
		if page == budgetMaxPages {
			rows = append(rows, quota{Provider: "databricks", Account: p.Name, Note: "budget list truncated at page cap"})
		}
	}
	return rows, nil
}

func budgetGet(ctx context.Context, p *providers.Provider, u string, into any) error {
	status, body, _, err := httpx.Do(ctx, httpx.Call{Client: http.DefaultClient, URL: u, Limit: 4 << 20,
		Authorize: func(h http.Header) error { return providers.Authorize(ctx, p, h) }})
	if status == 0 {
		return err
	}
	st := fmt.Sprintf("%d %s", status, http.StatusText(status))
	if status == http.StatusNotFound || status == http.StatusForbidden {
		return fmt.Errorf("%w (%s)", errBudgetAbsent, st)
	}
	if status != http.StatusOK {
		return fmt.Errorf("budget statuses: %s", st)
	}
	return json.Unmarshal(body, into)
}

// parseBudgetNum parses a gateway string number (commas and spaces stripped); empty, NaN and Inf are rejected.
func parseBudgetNum(s string) (float64, bool) {
	s = strings.NewReplacer(",", "", " ", "").Replace(s)
	v, err := strconv.ParseFloat(s, 64)
	if s == "" || err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// budgetMalformedNote marks a budget alert whose numbers did not parse: shown, never dropped, never cached.
const budgetMalformedNote = "unavailable: malformed budget row"

// budgetRows maps one budget's alert statuses to quota rows (USD), e.g. PER_USER/MONTH → "per-user", "month".
func budgetRows(profile string, s budgetStatus) []quota {
	var out []quota
	for _, a := range s.Alerts {
		pct, ok1 := parseBudgetNum(a.Consumed)
		spend, ok2 := parseBudgetNum(a.Spend)
		limit, ok3 := parseBudgetNum(a.Limit)
		if a.Spend == "" && a.Limit == "" { // a shared threshold withholds spend and threshold, sending percent_consumed only
			spend, limit, ok2, ok3 = 0, 0, true, true
		}
		if !ok1 || !ok2 || !ok3 {
			out = append(out, quota{Provider: "databricks", Account: profile, Note: budgetMalformedNote + " (" + s.Name + ")"})
			continue
		}
		scope := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(a.Scope, "ALERT_CONFIGURATION_SCOPE_TYPE_"), "_", "-"))
		out = append(out, quota{Provider: "databricks", Account: profile, Plan: scope,
			Window: strings.ToLower(a.Period), UsedPct: pct, Used: spend, Limit: limit, Unit: "USD"})
	}
	return out
}
