package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/copilotauth"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/serving"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
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
	reg.Providers["qa"].Auth.Type = providers.AuthNone // the pool skips dev (no stored credential) and lands on qa
	cells := []struct{ agent, profile, model, wire string }{
		{"claude", "dev", "claude-opus-5-5", "databricks-claude-opus-5-5"},
		{"claude", "ultra", "claude-opus-5-5", "claude-opus-5-5"},
		{"codex", "qa", "gpt-6-1-sol", "databricks-gpt-6-1-sol"},
		{"codex", "chatgpt", "gpt-6-1-sol", "gpt-6.1-sol"},
		{"copilot", "aueric_boehcp", "claude-opus-5.5", "claude-opus-5.5"},
		{"copilot", "aueric_boehcp", "opus", "gpt-6-astra"},
		{"copilot", "dev", "claude-opus-5-5", "databricks-claude-opus-5-5"},
		{"codex", "openai", "opus", "gpt-5"},
		{"gemini", "dev", "claude-sonnet-5-5", "databricks-claude-sonnet-5-5"},
		{"gemini", "antigravity", "claude-sonnet-5-5-medium", "claude-sonnet-5-5"},
		{"gemini", "antigravity", "gpt-oss-120b", "gpt-oss-120b"},
		{"gemini", "antigravity", "gemini-3-8-flash-high", "gemini-3.8-flash"},
	}
	for _, c := range cells {
		r, err := ResolveModel(reg, ModelQuery{Agent: c.agent, Profile: c.profile, Model: c.model}, time.Now())
		if err != nil || r.Profile != c.profile || r.Model != c.wire {
			t.Errorf("%s --profile %s %s = %+v, %v; want %s %s", c.agent, c.profile, c.model, r, err, c.profile, c.wire)
		}
	}
}

func familyWire(t *testing.T, agent, row, model string) (string, error) {
	t.Helper()
	r, err := ResolveModel(routeRegistry(t), ModelQuery{Agent: agent, Profile: row, Model: model}, time.Now())
	return r.Model, err
}

// A typed family is the row's highest listed version, else today's id; an unknown name errors like an unknown alias.
func TestResolveModelFamily(t *testing.T) {
	vaulttest.Use(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	cache := func(body string) {
		if err := os.WriteFile(filepath.Join(codexHome, "models_cache.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	check := func(agent, row, model, want string) {
		t.Helper()
		if got, err := familyWire(t, agent, row, model); err != nil || got != want {
			t.Errorf("%s --profile %s %s = %q, %v; want %s", agent, row, model, got, err, want)
		}
	}
	check("codex", "dev", "sol", "databricks-gpt-6-1-sol")
	if err := serving.Save(serving.Served{Profile: "dev", FetchedAt: time.Now(), Models: []serving.ServedModel{
		{ID: "databricks-gpt-6-sol"}, {ID: "databricks-gpt-6-1-sol"}, {ID: "databricks-gpt-6-2-sol"}}}); err != nil {
		t.Fatal(err)
	}
	check("codex", "dev", "sol", "databricks-gpt-6-2-sol")
	check("codex", "dev", "databricks-gpt-6-1-sol", "databricks-gpt-6-1-sol")
	copilotauth.StubRead(t, func(string) []string {
		return []string{"gpt-6.1-sol", "gpt-6.2-sol", "claude-opus-5.5"}
	}, nil)
	check("copilot", "aueric_boehcp", "sol", "gpt-6.2-sol")
	copilotauth.StubRead(t, nil, nil)
	check("copilot", "aueric_boehcp", "sol", "gpt-6.1-sol")
	check("codex", "chatgpt", "sol", "gpt-6.1-sol")
	cache(`{"models":[{"slug":"gpt-6.1-sol"},{"slug":"gpt-6.2-sol"}]}`)
	check("codex", "chatgpt", "sol", "gpt-6.2-sol")
	cache(`{not json`)
	check("codex", "chatgpt", "sol", "gpt-6.1-sol")
	_, errFamily := familyWire(t, "codex", "dev", "vega")
	_, errAlias := familyWire(t, "codex", "dev", "no-such-model")
	if !errors.Is(errFamily, ErrModelRoute) || strings.ReplaceAll(errFamily.Error(), "vega", "X") != strings.ReplaceAll(errAlias.Error(), "no-such-model", "X") {
		t.Errorf("unknown family err = %v, want the unknown-alias error %v", errFamily, errAlias)
	}
}

func TestResolveModelMarkedAlias(t *testing.T) {
	const wire = "claude-opus-4-8"
	p := &providers.Provider{Name: "dev", Provider: "anthropic", Protocol: providers.ProtocolAnthropic, BaseURL: "https://example.invalid",
		Models: map[string]string{models.Opus: wire + "[1m]"}}
	reg := &providers.Registry{Providers: map[string]*providers.Provider{"dev": p}}
	t.Setenv("CLAUDE_CODE_DISABLE_1M_CONTEXT", "")
	for _, c := range []struct {
		input string
		oneM  bool
	}{
		{"opus[1m]", true},
		{"opus", false},
		{wire + "[1m]", true},
	} {
		t.Run(c.input, func(t *testing.T) {
			r, err := ResolveModel(reg, ModelQuery{Agent: "claude", Model: c.input, Profile: "dev"}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			sel := providers.Selection{Provider: p, Aliases: map[string]providers.Target{
				models.Opus: {Provider: p, Model: wire},
			}}
			applyRoute(&sel, r, LaunchOptions{Model: c.input}, true)
			target := sel.Aliases[models.Opus]
			window := providers.Window(p, target.ClientModel())
			wantWindow := models.ContextWindowStd
			if c.oneM {
				wantWindow = models.ContextWindow1M
			}
			routeOneM := r.ContextTokens == models.ContextWindow1M
			t.Logf("route=%s tokens=%d; target=%s tokens=%d window=%d", r.Model, r.ContextTokens, target.Model, target.ContextTokens, window)
			if r.Model != wire || routeOneM != c.oneM || target.Model != wire || models.Has1M(target.ClientModel()) != c.oneM || window != wantWindow {
				t.Errorf("want wire=%s oneM=%v window=%d", wire, c.oneM, wantWindow)
			}
		})
	}
}

// A pin on the agent's own sign-in row decides over the session lead; any other pin keeps the session.
func TestResolveModelOwnLoginPinIgnoresSession(t *testing.T) {
	reg := routeRegistry(t)
	for _, c := range []struct{ agent, pin, session, profile string }{
		{"copilot", "aueric_boehcp", "prod", "aueric_boehcp"},
		{"codex", "dev", "prod", "prod"},
	} {
		r, err := ResolveModel(reg, ModelQuery{Agent: c.agent, PinProfile: c.pin, Session: c.session}, time.Now())
		if err != nil || r.Profile != c.profile {
			t.Errorf("%s pin %s session %s = %+v, %v; want %s", c.agent, c.pin, c.session, r, err, c.profile)
		}
	}
}

func TestResolveModelRefuses(t *testing.T) {
	reg := routeRegistry(t)
	for _, c := range [][3]string{
		{"claude", "", "gemini-4-argon-high"}, {"codex", "", "claude-opus-5-5"}, {"codex", "ultra", "claude-opus-5-5"},
		{"claude", "ultra", "databricks-claude-opus-5-5"}, {"gemini", "dev", "gpt-6-1-sol"},
		{"gemini", "antigravity", "gpt-oss-120b-high"}, {"gemini", "antigravity", "claude-sonnet-5-5-xhigh"},
		{"gemini", "antigravity", "gemini-3-8-flash-xhigh"}, {"gemini", "antigravity", "gemini-4-argon-xhigh"},
		{"gemini", "antigravity", "databricks-claude-sonnet-5-5"}, {"gemini", "antigravity", "claude-haiku-5-5"},
	} {
		if _, err := ResolveModel(reg, ModelQuery{Agent: c[0], Profile: c[1], Model: c[2], Session: "prod"}, time.Now()); !errors.Is(err, ErrModelRoute) {
			t.Errorf("%s --profile %s %s: err = %v; want ErrModelRoute", c[0], c[1], c[2], err)
		}
	}
	if r, err := ResolveModel(reg, ModelQuery{Agent: "gemini", Profile: "dev", Model: "opus", Session: "prod"}, time.Now()); err != nil || r.Model != "databricks-gemini-4-argon" {
		t.Errorf("gemini --profile dev opus = %+v, %v; want databricks-gemini-4-argon", r, err)
	}
}

// A typed or shell-pinned id a fresh listing lacks is refused with nearest ids; the pin names its variable.
func TestSpellModelAccepts(t *testing.T) {
	setConfigDir(t)
	reg := routeRegistry(t)
	saveListing(t, "dev", []string{"databricks-gpt-6-1-sol"})
	_, err := ResolveModel(reg, ModelQuery{Agent: "codex", Model: "bogus-model-xyz", Profile: "dev"}, time.Now())
	if ue, ok := errors.AsType[*UnacceptedError](err); !ok || !errors.Is(err, ErrModelRoute) || len(ue.Nearest) == 0 {
		t.Errorf("typed: err = %v; want an UnacceptedError with nearest ids", err)
	}
	ctx := proc.WithValues(t.Context(), proc.Values{"CODEX_MODEL": "bogus-model-xyz"})
	q := ModelQuery{ctx: ctx, Agent: "codex", Profile: "dev", PinModel: "bogus-model-xyz"}
	if _, err := ResolveModel(reg, q, time.Now()); !errors.Is(err, ErrModelRoute) || !strings.Contains(err.Error(), "CODEX_MODEL=") {
		t.Errorf("env pin: err = %v; want a refusal naming CODEX_MODEL", err)
	}
}

// agy's four Hydrogen efforts all resolve, and the refusal names the accepted ones.
func TestResolveModelAgyEfforts(t *testing.T) {
	reg := routeRegistry(t)
	for _, e := range []string{"low", "medium", "high", "xhigh"} {
		r, err := ResolveModel(reg, ModelQuery{Agent: "gemini", Profile: "antigravity", Model: "gemini-4-hydrogen-" + e}, time.Now())
		if err != nil || r.Effort != e || r.Model != "gemini-4-hydrogen" {
			t.Errorf("hydrogen-%s = %+v, %v", e, r, err)
		}
	}
	_, err := ResolveModel(reg, ModelQuery{Agent: "gemini", Profile: "antigravity", Model: "gpt-oss-120b-high"}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "takes effort medium") {
		t.Errorf("gpt-oss-120b-high err = %v; want the accepted levels named", err)
	}
}

// gemini's auto candidates: any chat model on a Databricks row, only Gemini on its Google login.
func TestAgentRunsGemini(t *testing.T) {
	reg := routeRegistry(t)
	if !agentRuns("gemini", reg.Providers["dev"], "databricks-claude-sonnet-5-5") ||
		agentRuns("gemini", reg.Providers["antigravity"], "claude-sonnet-5-5") {
		t.Error("gemini must run a non-Gemini model on a Databricks row and only Gemini on its Google login")
	}
}

func TestModelRouteOverlay(t *testing.T) {
	r := ModelRoute{Provider: "github", Profile: "business", Login: "aueric_boehcp", Model: "gpt-6-astra"}
	if o := r.Overlay(); o.Account != "aueric_boehcp" || o.Model != "gpt-6-astra" || r.Label() != "gpt-6-astra" {
		t.Errorf("overlay %+v label %s", o, r.Label())
	}
}

func TestResolveModelCodexAliasUsesRecordModel(t *testing.T) {
	reg := routeRegistry(t)
	for _, alias := range []string{"opus", "sonnet"} {
		q := ModelQuery{Agent: "codex", Model: alias, Profile: "dev", PinModel: "databricks-gpt-6-1-sol"}
		if r, err := ResolveModel(reg, q, time.Now()); err != nil || r.Model != "databricks-gpt-6-1-sol" {
			t.Errorf("codex %s = %+v, %v; want databricks-gpt-6-1-sol", alias, r, err)
		}
	}
}

// agy's own models typed on the Google row keep their id (never the tier alias) and refuse an effort agy lacks.
func TestAgyOwnTarget(t *testing.T) {
	g := routeRegistry(t).Providers["antigravity"]
	for _, c := range []struct{ model, want string }{
		{"claude-sonnet-5-5-medium", "claude-sonnet-5-5-medium"}, {"claude-sonnet-5-5", "claude-sonnet-5-5"},
		{"gpt-oss-120b", "gpt-oss-120b"}, {"gemini-3-8-flash-high", "gemini-3.8-flash-high"},
		{"gemini-4-hydrogen-xhigh", "gemini-4-hydrogen-xhigh"},
	} {
		tg, ok, err := agyOwnTarget(RunOptions{Agent: "gemini", Model: c.model}, g)
		if !ok || err != nil || tg.Model != c.want {
			t.Errorf("%s = %q, %v, %v; want %q", c.model, tg.Model, ok, err, c.want)
		}
	}
	for _, m := range []string{"gpt-oss-120b-high", "claude-sonnet-5-5-xhigh", "gemini-3-8-flash-xhigh"} {
		if _, _, err := agyOwnTarget(RunOptions{Agent: "gemini", Model: m}, g); err == nil || !strings.Contains(err.Error(), "takes effort") {
			t.Errorf("%s: err = %v; want the accepted levels named", m, err)
		}
	}
	if _, ok, _ := agyOwnTarget(RunOptions{Agent: "claude", Model: "claude-sonnet-5-5-low"}, g); ok {
		t.Error("only the gemini agent takes agy's own models")
	}
}

// With no --profile a databricks- model takes a gateway row, a Gemini model the Google row unless a gateway pin holds.
func TestResolveModelGeminiNoProfile(t *testing.T) {
	reg := routeRegistry(t)
	for _, c := range []struct{ name, model, session, pin, profile string }{
		{"databricks- takes the session row over the Google pin", "databricks-claude-sonnet-5-5", "dev", "antigravity", "dev"},
		{"databricks- takes a gateway pin with no session", "databricks-claude-sonnet-5-5", "", "qa", "qa"},
		{"Gemini model takes Google over the session row", "gemini-4-hydrogen-medium", "dev", "", "antigravity"},
		{"Gemini model follows a gateway pin", "gemini-4-hydrogen-medium", "dev", "qa", "dev"},
		{"claude-* keeps the pin in force", "claude-sonnet-5-5", "dev", "antigravity", "antigravity"},
		{"claude-* keeps the session when unpinned", "claude-sonnet-5-5", "dev", "", "dev"},
	} {
		r, err := ResolveModel(reg, ModelQuery{Agent: "gemini", Model: c.model, Session: c.session, PinProfile: c.pin}, time.Now())
		if err != nil || r.Profile != c.profile {
			t.Errorf("%s: got %s, %v; want %s", c.name, r.Profile, err, c.profile)
		}
	}
	q := ModelQuery{Agent: "gemini", Model: "databricks-claude-sonnet-5-5", Profile: "antigravity", Session: "dev"}
	if _, err := ResolveModel(reg, q, time.Now()); !errors.Is(err, ErrModelRoute) {
		t.Errorf("explicit --profile antigravity must win and refuse a databricks- model: %v", err)
	}
	reg.Default = "antigravity"
	q = ModelQuery{Agent: "gemini", Model: "databricks-claude-sonnet-5-5", PinProfile: "antigravity"}
	if _, err := ResolveModel(reg, q, time.Now()); !errors.Is(err, ErrModelRoute) {
		t.Errorf("no Databricks row anywhere must refuse, got %v", err)
	}
}

func TestCommandTokenPoolHonorsDispatchOverride(t *testing.T) {
	vaulttest.Use(t)
	reg := routeRegistry(t)
	p := reg.Providers["dev"]
	p.Auth.Type = providers.AuthNone
	dir := filepath.Join(paths.StateDir(), "cache", "token-expiry")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	raw := fmt.Sprintf(`{"schema":2,"exp":%d}`, now.Add(14*time.Minute).Unix())
	if err := os.WriteFile(filepath.Join(dir, p.Name+".json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	q := ModelQuery{Agent: "codex", Model: "gpt-6-1-sol", Profile: poolName}
	if r, err := ResolveModel(reg, q, now); err != nil || r.Profile != "dev" {
		t.Fatalf("refreshable pool = %v, %v", r, err)
	}
	ctx := proc.WithValues(t.Context(), proc.Values{route.OverlayVar: `{"tokenCommand":[]}`})
	q = QueryFor(ctx, q)
	if _, err := ResolveModel(reg, q, now); err == nil {
		t.Fatal("disabled command accepted short token")
	}
}

func TestResolveModelKeepsRouteWhole(t *testing.T) {
	vaulttest.Use(t)
	reg := routeRegistry(t)
	pinned := route.Route{Provider: "databricks", Profile: "dev", Identity: "sp", Host: "worker"}
	vaulttest.Put(t, vault.Overlay, vault.AgentRecordsKey,
		`{"claude":{"route":{"provider":"databricks","profile":"dev","identity":"sp","host":"worker"},"profile":"ultra"}}`)
	q := QueryFor(t.Context(), ModelQuery{Agent: "claude", Model: "claude-opus-5-5"})
	if q.PinRoute != pinned || q.PinProfile != pinned.Profile {
		t.Fatalf("pin = %+v, %q; want %+v", q.PinRoute, q.PinProfile, pinned)
	}
	q.Session = ""
	r, err := ResolveModel(reg, q, time.Now())
	if err != nil || r.Overlay().Route() != pinned {
		t.Fatalf("pinned = %+v, %v; want %+v", r, err, pinned)
	}
	want := route.Route{Provider: "anthropic", Profile: "ultra"}
	q.Route = want
	r, err = ResolveModel(reg, q, time.Now())
	if err != nil || r.Overlay().Route() != want {
		t.Fatalf("overlay route = %+v, %v; want %+v", r, err, want)
	}
}

func agentRuns(agentName string, p *providers.Provider, wire string) bool {
	return agentRunsIn(nil, agentName, p, wire)
}
