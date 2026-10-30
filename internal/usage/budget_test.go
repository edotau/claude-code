package usage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

func budgetServer(t *testing.T, hits *atomic.Int32, handler http.HandlerFunc) *providers.Provider {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("BUDGET_TEST_TOKEN", "tok")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	prev := http.DefaultClient.Transport
	http.DefaultClient.Transport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultClient.Transport = prev; srv.Close() })
	return &providers.Provider{Name: "dev", Preset: providers.PresetDatabricks, Host: srv.URL,
		Auth: providers.Auth{Type: providers.AuthKey, Env: "BUDGET_TEST_TOKEN"}}
}

const budgetPage1 = `{"budget_spend_statuses":[{"name":"b1","alert_spend_statuses":[{"scope_type":"ALERT_CONFIGURATION_SCOPE_TYPE_PER_USER","time_period":"MONTH","spend":"100.6","threshold":"100","percent_consumed":"100.6"}]}],"next_page_token":"p2"}`
const budgetPage2 = `{"budget_spend_statuses":[{"name":"b2","alert_spend_statuses":[{"scope_type":"ALERT_CONFIGURATION_SCOPE_TYPE_WORKSPACE","time_period":"MONTH","spend":"5","threshold":"50","percent_consumed":"10"}]}]}`

func TestBudgetQuotasPagesMapsAndCaches(t *testing.T) {
	var hits atomic.Int32
	p := budgetServer(t, &hits, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ai-gateway/v2/budget-spend-statuses" || r.URL.Query().Get("page_size") != "100" {
			http.NotFound(w, r)
			return
		}
		body := budgetPage1
		if r.URL.Query().Get("page_token") == "p2" {
			body = budgetPage2
		}
		_, _ = w.Write([]byte(body))
	})
	now := time.Now()
	rows := budgetQuotas(context.Background(), p, now)
	if len(rows) != 2 || hits.Load() != 2 {
		t.Fatalf("rows=%+v hits=%d, want 2 rows over 2 pages", rows, hits.Load())
	}
	if r := rows[0]; r.Plan != "per-user" || r.Window != "month" || r.UsedPct != 100.6 || r.Used != 100.6 || r.Limit != 100 || r.Unit != "USD" {
		t.Errorf("row 0 = %+v", r)
	}
	setStates(rows, 80)
	if rows[0].State != "exceeded" || rows[1].State != "ok" {
		t.Errorf("states = %q, %q", rows[0].State, rows[1].State)
	}
	if again := budgetQuotas(context.Background(), p, now.Add(time.Minute)); len(again) != 2 || hits.Load() != 2 {
		t.Errorf("fresh cache refetched: rows=%d hits=%d", len(again), hits.Load())
	}
	if budgetQuotas(context.Background(), p, now.Add(budgetCacheTTL+time.Second)); hits.Load() != 4 {
		t.Errorf("expired cache not refetched: hits=%d", hits.Load())
	}
}

func TestBudgetQuotasMissingEndpointYieldsNoRows(t *testing.T) {
	var hits atomic.Int32
	for _, code := range []int{http.StatusNotFound, http.StatusForbidden} {
		p := budgetServer(t, &hits, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
		if rows := budgetQuotas(context.Background(), p, time.Now()); rows != nil {
			t.Errorf("%d: rows = %+v, want none", code, rows)
		}
		if readBudgetCache(budgetCachePath()) != nil {
			t.Errorf("%d: a failed fetch was cached", code)
		}
	}
}

func TestBudgetRowsSkipUnparsableNumbers(t *testing.T) {
	mk := func(spend, limit, pct string) budgetStatus {
		var s budgetStatus
		s.Alerts = append(s.Alerts, struct {
			Scope    string `json:"scope_type"`
			Period   string `json:"time_period"`
			Spend    string `json:"spend"`
			Limit    string `json:"threshold"`
			Consumed string `json:"percent_consumed"`
		}{"X", "MONTH", spend, limit, pct})
		return s
	}
	for _, bad := range [][3]string{{"", "100", "5"}, {"1", "100", "NaN"}, {"1", "Inf", "5"}, {"1", "1e999", "5"}} {
		if rows := budgetRows("dev", mk(bad[0], bad[1], bad[2])); len(rows) != 0 {
			t.Errorf("%v produced rows %+v, want none", bad, rows)
		}
	}
	rows := budgetRows("dev", mk(" 1,100.5 ", "2,000", "50.2"))
	if len(rows) != 1 || rows[0].Used != 1100.5 || rows[0].Limit != 2000 {
		t.Errorf("comma values = %+v", rows)
	}
}
