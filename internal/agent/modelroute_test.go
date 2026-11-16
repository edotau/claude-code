package agent

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

const routeOverlay = `{"providers":{"databricks":{
 "dev":{"auth":{"type":"oauth"},"host":"https://dev.cloud.databricks.com","preset":"databricks"},
 "qa":{"auth":{"type":"oauth"},"host":"https://qa.cloud.databricks.com","preset":"databricks"},
 "prod":{"auth":{"type":"oauth"},"host":"https://prod.cloud.databricks.com","preset":"databricks"}},
 "github":{"business":{"auth":{"login":"copilot","type":"cli"},"base_url":"https://api.githubcopilot.com","login":"aueric_boehcp",
  "models":{"haiku":"claude-haiku-5.5","opus":"gpt-6-astra","sonnet":"kimi-k3"},"protocol":"openai"}}}}`

// routeRegistry is the shipped defaults plus the live dev/qa/prod and business rows.
func routeRegistry(t *testing.T) *providers.Registry {
	t.Helper()
	base, err := os.ReadFile("../providers/defaults.json")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Parse(base, []byte(routeOverlay))
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestResolveModel(t *testing.T) {
	reg := routeRegistry(t)
	cells := []struct{ agent, model, profile, wire string }{
		{"claude", "dev:claude-opus-5-5", "dev", "databricks-claude-opus-5-5"},
		{"claude", "ultra:claude-opus-5-5", "ultra", "claude-opus-5-5"},
		{"codex", "qa:gpt-6-1-sol", "qa", "databricks-gpt-6-1-sol"},
		{"codex", "chatgpt:gpt-6-1-sol", "chatgpt", "gpt-6.1-sol"},
		{"copilot", "business:claude-opus-5.5", "business", "claude-opus-5.5"},
		{"copilot", "business:opus", "business", "gpt-6-astra"},
		{"copilot", "dev:claude-opus-5-5", "dev", "databricks-claude-opus-5-5"},
	}
	for _, c := range cells {
		r, err := ResolveModel(reg, ModelQuery{Agent: c.agent, Model: c.model}, time.Now())
		if err != nil || r.Profile != c.profile || r.Model != c.wire {
			t.Errorf("%s %s = %+v, %v; want %s %s", c.agent, c.model, r, err, c.profile, c.wire)
		}
	}
}

func TestResolveModelRefuses(t *testing.T) {
	reg := routeRegistry(t)
	for _, c := range [][2]string{
		{"claude", "gemini-4-argon-high"}, {"codex", "claude-opus-5-5"}, {"codex", "ultra:claude-opus-5-5"},
	} {
		if _, err := ResolveModel(reg, ModelQuery{Agent: c[0], Model: c[1], Session: "prod"}, time.Now()); !errors.Is(err, ErrModelRoute) {
			t.Errorf("%s %s: err = %v; want ErrModelRoute", c[0], c[1], err)
		}
	}
}

func TestModelRouteOverlay(t *testing.T) {
	r := ModelRoute{Provider: "github", Profile: "business", Model: "gpt-6-astra"}
	if o := r.Overlay(); o.Account != "business" || o.Model != "gpt-6-astra" || r.Label() != "gpt-6-astra" {
		t.Errorf("overlay %+v label %s", o, r.Label())
	}
}

func TestResolveModelCodexAliasUsesRecordModel(t *testing.T) {
	reg := routeRegistry(t)
	for _, alias := range []string{"opus", "sonnet"} {
		q := ModelQuery{Agent: "codex", Model: "dev:" + alias, PinModel: "databricks-gpt-6-1-sol"}
		if r, err := ResolveModel(reg, q, time.Now()); err != nil || r.Model != "databricks-gpt-6-1-sol" {
			t.Errorf("codex %s = %+v, %v; want databricks-gpt-6-1-sol", alias, r, err)
		}
	}
}

func TestResolveModelInteractiveConflict(t *testing.T) {
	q := ModelQuery{Agent: "claude", Model: "ultra:claude-opus-5-5", Profile: "dev", Interactive: true}
	_, err := ResolveModel(routeRegistry(t), q, time.Now())
	if !errors.Is(err, ErrModelRoute) || !strings.Contains(err.Error(), "--profile dev") || !strings.Contains(err.Error(), "ultra:") {
		t.Errorf("err = %v; want a route error naming --profile dev and the ultra: prefix", err)
	}
}
