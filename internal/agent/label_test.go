package agent

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
	endpoints "github.com/edotau/claude-code/internal/serving"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

type labelEngine struct{ stubEngine }

func (labelEngine) Run(context.Context, RunOptions, wire.Sink) (wire.Result, error) {
	return wire.Result{}, nil
}

// A ladder hop labels the model its engine runs: a pinned cell as typed, an alias cell the row's own pick (opus).
func TestLadderHopIsTheRunModel(t *testing.T) {
	reg := routeRegistry(t)
	for _, tc := range []struct {
		name, want, row, cell, account, wantModel string
		keepCaller                                bool
	}{
		{"codex cell", "codex", "prod", "databricks-gpt-6-1-sol", "databricks·prod", "databricks-gpt-6-1-sol", false},
		{"opencode alias cell", "opencode", "", "sonnet", "databricks·dev", "databricks-claude-opus-5-5", false},
		{"opencode keeps caller", "opencode", "dev", "databricks-claude-sonnet-5-5", "databricks·dev", "databricks-claude-opus-5-5", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Dispatch{Agent: labelEngine{stubEngine{name: tc.want}}, Model: tc.cell, Route: rowRoute(reg, tc.row), Account: tc.account, keepCaller: tc.keepCaller}
			hop, note, err := ladderHop(reg, ModelQuery{Agent: tc.want}, d)
			if err != nil || note != "" {
				t.Fatalf("hop = %+v, %q, %v", hop, note, err)
			}
			r, err := ResolveModel(reg, hop, time.Now())
			if err != nil || r.Label() != tc.wantModel {
				t.Errorf("hop %+v labels %q (%v), want %q", hop, r.Label(), err, tc.wantModel)
			}
		})
	}
	d := Dispatch{Agent: labelEngine{stubEngine{name: "claude"}}, Model: "sonnet", Account: "anthropic·nowhere"}
	if hop, note, _ := ladderHop(reg, ModelQuery{Agent: "codex"}, d); hop.Agent != "claude" || hop.Model != "sonnet" || hop.Profile != "" || note == "" {
		t.Errorf("fallback hop = %+v %q, want claude on sonnet, no unknown profile, and a note", hop, note)
	}
}

// A typed model, profile or overlay row is the route as written: no ladder runs.
func TestRunQueryTypedSkipsLadder(t *testing.T) {
	for _, q := range []ModelQuery{{Agent: "codex", Model: "gpt-6-1-sol"}, {Agent: "opencode", Profile: "dev"},
		{Agent: "copilot", Account: "x"}, {Agent: "claude"}} {
		if got, note, err := RunQuery(context.Background(), routeRegistry(t), q, "haiku"); got != q || note != "" || err != nil {
			t.Errorf("RunQuery(%+v) = %+v %q %v, want it unchanged", q, got, note, err)
		}
	}
}

func TestStoredPoolPinWorkspace(t *testing.T) {
	setConfigDir(t)
	vaulttest.Use(t)
	reg := &providers.Registry{Providers: map[string]*providers.Provider{
		"a-unsigned": {Name: "a-unsigned", Provider: "databricks", Preset: "databricks", Auth: providers.Auth{Type: providers.AuthOAuth}},
		"b-first":    {Name: "b-first", Provider: "databricks", Preset: "databricks", Auth: providers.Auth{Type: providers.AuthNone}},
		"qa":         {Name: "qa", Provider: "databricks", Preset: "databricks"},
		"login":      {Name: "login", Provider: "openai"},
	}}
	for _, pin := range []string{`{"profile":"databricks"}`, `{"route":{"provider":"databricks","profile":"auto"}}`} {
		vaulttest.Put(t, vault.Overlay, vault.AgentRecordsKey, `{"codex":`+pin+`}`)
		for _, tc := range []struct{ session, typed, want string }{
			{"qa", "", "qa"}, {"login", "", "b-first"}, {"", "", "b-first"},
			{"qa", "auto", "auto"}, {"qa", "b-first", "b-first"},
		} {
			t.Setenv(providers.EnvSessionProfile, tc.session)
			q := QueryFor(context.Background(), ModelQuery{Agent: "codex", Model: AutoModel})
			if tc.typed != "" {
				q.Provider, q.Profile = "databricks", tc.typed
			}
			got, _, err := RunQuery(context.Background(), reg, q, "")
			if err != nil || got.Profile != tc.want || got.Provider != "databricks" {
				t.Fatalf("pin %s, session %q, typed %q: route %s/%s, err %v; want databricks/%s",
					pin, tc.session, tc.typed, got.Provider, got.Profile, err, tc.want)
			}
			if vault.GetAgentRecord("codex").EffectiveRoute() != poolRoute {
				t.Fatal("stored pin was rewritten")
			}
		}
	}
}

// A typed alias on gemini is the Claude id agy serves, else the row's tier; other agents keep the alias.
func TestRunQueryAgyAlias(t *testing.T) {
	for _, c := range []struct{ agent, model, want string }{
		{"gemini", "opus", "claude-opus-5-5"}, {"gemini", "sonnet", "claude-sonnet-5-5"},
		{"gemini", "haiku", "haiku"}, {"codex", "opus", "opus"}, {"copilot", "opus", "opus"},
	} {
		got, _, err := RunQuery(context.Background(), &providers.Registry{}, ModelQuery{Agent: c.agent, Model: c.model}, "")
		if err != nil || got.Model != c.want {
			t.Errorf("%s --model %s = %q, %v; want %q", c.agent, c.model, got.Model, err, c.want)
		}
	}
}

// TaskTier reads a Tier: line first, then deep, fast and standard signals from the work text alone.
func TestTaskTier(t *testing.T) {
	for _, tc := range []struct{ brief, want string }{
		{"Fix the typo in internal/agent/label.go.", "haiku"},
		{"Move internal/a.go to internal/b/a.go.", "haiku"},
		{"Add a --json flag to agents status in internal/cli/agents.go and internal/agent/status.go.", "sonnet"},
		{"Audit the safety-scan rules for false blocks.", "opus"},
		{"Write the parser for the Run: grammar.", "opus"},
		{"Tier: fast\nAudit the safety-scan rules.", "haiku"},
		{"Fix the typo.\nTier: deep", "opus"},
		{"Run: claude-code agents ask --agent opencode --profile dev\nRename one variable in x.go.", "haiku"},
		{"Rename one variable.\nVerify: `claude-code review simplify --fail` and an audit of nothing", "haiku"},
		{"Rename the helper across the codebase.", "sonnet"},
		{"Rename the database column and migrate existing customer data in migration.sql.", "opus"},
		{"Debug the production deadlock in worker.go; move the lock acquisition after the channel send.", "sonnet"},
		{"Implement transaction rollback in store.go and reword the error message.", "sonnet"},
		{"Investigate why the cache returns stale entries after we rename keys in cache.go.", "sonnet"},
		{"Rewrite the session cookie check in session.go so a replayed token fails; move it first.", "sonnet"},
		{"Run go test ./internal/x and report the failures.", "haiku"},
		{"Run go test ./internal/x and fix the failing test.", "sonnet"},
		{"Count the exported funcs in x.go.", "haiku"},
		{"", ""},
	} {
		if got, sig := TaskTier(tc.brief); got != tc.want {
			t.Errorf("TaskTier(%q) = %q (%s), want %q", tc.brief, got, sig, tc.want)
		}
	}
}

// LookupTier takes haiku for a plain sweep or lookup and leaves thorough, edit and tiered briefs to TaskTier.
func TestLookupTier(t *testing.T) {
	for _, tc := range []struct {
		brief      string
		lookupOnly bool
		want       string
	}{
		{"Find where Foo is defined.", false, "haiku"},
		{"Very thorough sweep of the hooks.", false, ""},
		{"Audit the auth paths.", false, ""},
		{"Look up the docs for the flag.", true, "haiku"},
		{"Look up the docs and fix the typo.", true, ""},
		{"Summarise the module.", true, ""},
		{"Tier: standard\nFind where Foo is defined.", false, ""},
	} {
		if got, sig := LookupTier(tc.brief, tc.lookupOnly); got != tc.want {
			t.Errorf("LookupTier(%q) = %q (%s), want %q", tc.brief, got, sig, tc.want)
		}
	}
}

// A tier the row's fresh listing shows not READY falls back to the untiered pick and says so.
func TestResolveRunTierNotServed(t *testing.T) {
	setConfigDir(t)
	reg := routeRegistry(t)
	now := time.Now()
	if err := endpoints.Save(endpoints.Served{Profile: "dev", FetchedAt: now, Models: []endpoints.ServedModel{
		{ID: "databricks-claude-opus-5-5", Ready: "READY"}}}); err != nil {
		t.Fatal(err)
	}
	q := ModelQuery{Agent: "opencode", Profile: "dev", Default: "haiku"}
	r, err := ResolveRun(reg, q, now)
	base, _ := ResolveModel(reg, ModelQuery{Agent: "opencode", Profile: "dev"}, now)
	if err != nil || r.Model != base.Model || !strings.Contains(r.Note, "task tier haiku is not served") {
		t.Errorf("ResolveRun = %+v, %v; want the untiered %s with a note", r, err, base.Model)
	}
	q.Default = "opus"
	if r, err := ResolveRun(reg, q, now); err != nil || r.Note != "" {
		t.Errorf("served tier = %+v, %v; want it with no note", r, err)
	}
}

// A typed catalog model a fresh listing lacks is refused under any fallback (R8, plan 2.1); no listing is unverified.
func TestResolveRunUnservedCatalogModel(t *testing.T) {
	setConfigDir(t)
	reg := routeRegistry(t)
	now := time.Now()
	q := ModelQuery{Agent: "gemini", Profile: "dev", Model: "gemini-4-argon-high"}
	if r, err := ResolveRun(reg, q, now); err != nil || r.Served != ServedUnverified {
		t.Errorf("no listing: %+v, %v; want Served %q", r, err, ServedUnverified)
	}
	saveListing(t, "dev", []string{"databricks-gemini-3-8-flash"})
	for _, fb := range []string{"", router.FallbackNone} {
		q.Fallback = fb
		if _, err := ResolveRun(reg, q, now); !errors.Is(err, ErrModelRoute) || !strings.Contains(err.Error(), "databricks-gemini-3-8-flash") {
			t.Errorf("fallback %q: err = %v; want ErrModelRoute naming the served alternative", fb, err)
		}
	}
	q.Model = "gemini-3-8-flash-high"
	if r, err := ResolveRun(reg, q, now); err != nil || r.Served != "" {
		t.Errorf("served model: %+v, %v; want it unmarked", r, err)
	}
	checkOwnLoginNotes(t, reg, now)
}

// checkOwnLoginNotes: claude on the ultra own-login row notes an unknown id as unverified, never a catalog id or alias.
func checkOwnLoginNotes(t *testing.T, reg *providers.Registry, now time.Time) {
	t.Helper()
	for model, want := range map[string]bool{"bogus-model-xyz": true, "claude-sonnet-5-5": false, "opus": false} {
		r, err := ResolveRun(reg, ModelQuery{Agent: "claude", Profile: "ultra", Model: model}, now)
		if got := strings.Contains(r.Note, "unverified: "+model+" is not in the catalog"); err != nil || got != want {
			t.Errorf("own-login %s: note %q, %v; want unverified note = %v", model, r.Note, err, want)
		}
	}
}

// autoOverlay is five Databricks rows (each a listing fixture) and a github row whose models are its own router.
const autoOverlay = `{"providers":{"databricks":{
 "dev":{"auth":{"type":"oauth"},"host":"https://dev.cloud.databricks.com","preset":"databricks"},
 "qa":{"auth":{"type":"oauth"},"host":"https://qa.cloud.databricks.com","preset":"databricks"},
 "prod":{"auth":{"type":"oauth"},"host":"https://prod.cloud.databricks.com","preset":"databricks"},
 "xf":{"auth":{"type":"oauth"},"host":"https://xf.cloud.databricks.com","preset":"databricks"},
 "ax":{"auth":{"type":"oauth"},"host":"https://ax.cloud.databricks.com","preset":"databricks"}},
 "github":{"router":{"auth":{"login":"copilot","type":"cli"},"base_url":"https://api.githubcopilot.com","login":"router",
  "models":{"haiku":"auto","opus":"auto","sonnet":"auto"},"protocol":"openai"}}}}`

// devListing is dev's served chat models; databricks-auto is an endpoint auto must never pick.
var devListing = []string{"databricks-claude-opus-5-5", "databricks-claude-sonnet-5-5", "databricks-claude-haiku-5-5",
	"databricks-kimi-k3", "databricks-gpt-6-1-sol", "databricks-gemini-3-8-flash", "databricks-gpt-5-4-nano", "databricks-auto"}

const (
	fastBrief     = "Fix the typo in internal/agent/label.go."
	standardBrief = "Add a --json flag to agents status in internal/cli/agents.go and internal/agent/status.go."
	deepBrief     = "Audit the safety-scan rules for false blocks."
)

func saveListing(t testing.TB, profile string, ids []string) {
	t.Helper()
	s := endpoints.Served{Profile: profile, FetchedAt: time.Now()}
	for _, id := range ids {
		s.Models = append(s.Models, endpoints.ServedModel{ID: id, Task: "llm/v1/chat", Ready: "READY"})
	}
	if err := endpoints.Save(s); err != nil {
		t.Fatal(err)
	}
}

// oracle is the ranking's own first pick of ids for tier, each probe marked by tools.
func oracle(ids []string, tier string, needTools bool, tools map[string]string) string {
	var cands []catalog.Candidate
	for _, id := range ids {
		if catalog.ID(id) != AutoModel {
			cands = append(cands, catalog.Candidate{Model: id, Tools: tools[id]})
		}
	}
	if r := catalog.Choose(cands, "opencode", tier, needTools).Ranked; len(r) > 0 {
		return r[0].Model
	}
	return ""
}

// crossFamily finds a non-Claude model ranked like sonnet but cheaper, so cost alone must decide.
func crossFamily(t testing.TB) (claude, other string) {
	t.Helper()
	claude = "databricks-claude-sonnet-5-5"
	s := catalog.RankOf(claude)
	for _, id := range []string{"kimi-k3", "glm-5-3", "gpt-6-1-terra", "grok-4-7", "gemini-3-8-flash", "deepseek-v4-pro-0813"} {
		if r := catalog.RankOf(id); r.Known && r.Capability == s.Capability && r.Speed == s.Speed && r.Cost < s.Cost {
			return claude, "databricks-" + id
		}
	}
	t.Fatal("ranks.json has no non-Claude model ranked like sonnet at a lower cost")
	return "", ""
}

// autoFixture loads autoOverlay and listings: dev full, qa below deep, xf a cross-family pair, ax unknowns, prod none.
func autoFixture(t testing.TB) (*providers.Registry, []string, []string) {
	t.Helper()
	setConfigDir(t)
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", autoOverlay)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	weak := []string{"databricks-gemini-3-8-flash", "databricks-gpt-5-4-nano"}
	need := catalog.Choose(nil, "", "deep", false).Need
	for _, id := range weak {
		if catalog.RankOf(id).Capability >= need {
			t.Fatalf("fixture: %s is deep-capable; qa must list only weaker models", id)
		}
	}
	claude, other := crossFamily(t)
	saveListing(t, "dev", devListing)
	saveListing(t, "qa", weak)
	saveListing(t, "xf", []string{claude, other})
	saveListing(t, "ax", []string{"databricks-auto", "databricks-zz-unranked"})
	return reg, weak, []string{claude, other}
}

// keyedAutoFixture is autoFixture with a dev credential, so the hop's host guard has one to check.
func keyedAutoFixture(t *testing.T) *providers.Registry {
	t.Helper()
	autoFixture(t)
	vaulttest.Put(t, vault.Overlay, "providers", strings.Replace(autoOverlay, `"dev":{"auth":{"type":"oauth"}`,
		`"dev":{"auth":{"type":"key","command":"echo qzvxplk"}`, 1))
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// rankedOnly holds the fresh rotation pool empty while implicit auto still ranks the served listing.
func rankedOnly(t testing.TB) {
	t.Helper()
	vaulttest.Put(t, vault.Overlay, "discovery-models", `["databricks-glm-5-3"]`)
}

// band is the rotation pool ids give tier by the ranking's own rule, each probe marked by probes.
func band(ids []string, tier string, tools bool, probes map[string]string) []string {
	var cands []catalog.Candidate
	for _, id := range ids {
		if catalog.ID(id) != AutoModel {
			cands = append(cands, catalog.Candidate{Model: id, Tools: probes[id]})
		}
	}
	return catalog.RotationPool(cands, tier, tools)
}

// An implicit or concrete auto ranks the served listing; a cold listing runs the row's tier model with a note.
func TestResolveRunAuto(t *testing.T) {
	reg, weak, pair := autoFixture(t)
	std := oracle(devListing, "standard", false, nil)
	pin, alias := "databricks-claude-haiku-5-5", "databricks-claude-sonnet-5-5"
	if std == alias {
		t.Fatalf("fixture has no teeth: standard %s is the alias", std)
	}
	strongest := slices.MaxFunc(weak, func(a, b string) int { return catalog.RankOf(a).Capability - catalog.RankOf(b).Capability })
	prodOpus, _ := ResolveModel(reg, ModelQuery{Agent: "opencode", Profile: "prod", Model: "opus"}, time.Now())
	for _, tc := range []struct{ name, agent, model, profile, pin, task, want, note string }{
		{"typed concrete", "opencode", pin, "dev", "", deepBrief, pin, ""},
		{"pin with nothing typed", "opencode", "", "dev", pin, deepBrief, pin, ""},
		{"no pin, nothing typed", "opencode", "", "dev", "", standardBrief, std, ""},
		{"implicit stepped down", "opencode", "", "qa", "", deepBrief, strongest, "stepped down"},
		{"implicit cross-family on cost", "opencode", "", "xf", "", standardBrief, pair[1], ""},
		{"implicit listed endpoint auto", "opencode", "", "ax", "", fastBrief, "databricks-zz-unranked", ""},
		{"asked auto, cold listing", "opencode", "auto", "prod", "", deepBrief, prodOpus.Model, "no fresh model listing"},
		{"registry model auto", "copilot", "auto", "router", "", standardBrief, "", "own router"},
	} {
		r, err := ResolveRun(reg, ModelQuery{Agent: tc.agent, Model: tc.model, Profile: tc.profile, PinModel: tc.pin, Task: tc.task}, time.Now())
		switch {
		case err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && r.Model != tc.want, r.Model == AutoModel, r.Model == "databricks-auto":
			t.Errorf("%s: ran %s, want %s (note %q)", tc.name, r.Model, cmp.Or(tc.want, "a concrete model"), r.Note)
		case !strings.Contains(r.Note, tc.note):
			t.Errorf("%s: note %q, want it to say %q", tc.name, r.Note, tc.note)
		}
	}
}

// An asked auto on a fresh listing picks inside the brief's tier band and says the tier; an empty band refuses.
func TestResolveRunAskedAuto(t *testing.T) {
	reg, weak, pair := autoFixture(t)
	std := oracle(devListing, "standard", false, nil)
	notools := map[string]string{std: "no"}
	if err := catalog.SaveCheckCache(catalog.CheckCache{Provider: "dev", Results: []serving.CheckResult{{ID: catalog.ID(std), Status: "ok", Tools: "no"}}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, row, model, pin, task, tier string
		ids                               []string
		tools                             bool
	}{
		{"typed auto over a pin", "dev", "auto", "databricks-claude-haiku-5-5", deepBrief, "deep", devListing, false},
		{"pinned auto", "dev", "", "auto", deepBrief, "deep", devListing, false},
		{"Tier: line wins", "dev", "auto", "", "Tier: fast\n" + deepBrief, "fast", devListing, false},
		{"safety-critical bump", "dev", "auto", "", "Fix the typo in the permission rule in label.go.", "deep", devListing, false},
		{"large mechanical", "dev", "auto", "", "Rename Foo to Bar across every caller in the codebase. " +
			strings.Repeat("Keep each call site's behaviour. ", 40), "standard", devListing, false},
		{"tools exclude", "dev", "auto", "", standardBrief, "standard", devListing, true},
		{"cross-family pair", "xf", "auto", "", standardBrief, "standard", pair, false},
	} {
		want := band(tc.ids, tc.tier, tc.tools, notools)
		r, err := ResolveRun(reg, ModelQuery{Agent: "opencode", Model: tc.model, Profile: tc.row, PinModel: tc.pin, Task: tc.task, Tools: tc.tools}, time.Now())
		switch {
		case len(want) == 0:
			t.Fatalf("%s: fixture has an empty %s band", tc.name, tc.tier)
		case err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case !slices.Contains(want, r.Model), r.Model == "databricks-auto", tc.tools && r.Model == std:
			t.Errorf("%s: ran %s, want one of %v (%s tier)", tc.name, r.Model, want, tc.tier)
		case !strings.Contains(r.Note, "("+tc.tier+" tier,"):
			t.Errorf("%s: note %q, want the %s tier", tc.name, r.Note, tc.tier)
		}
	}
	if got := band(weak, "deep", false, nil); len(got) != 0 {
		t.Fatalf("fixture: qa serves %v in the deep band", got)
	}
	if r, err := ResolveRun(reg, ModelQuery{Agent: "opencode", Profile: "qa", Model: "auto", Task: deepBrief}, time.Now()); !errors.Is(err, ErrPoolExhausted) {
		t.Errorf("fresh qa listing with nothing deep ran %s (%v), want ErrPoolExhausted", r.Model, err)
	}
}

// A typed auto reaches the run untyped with AutoModel set; a --provider row stays.
func TestFoldAuto(t *testing.T) {
	for _, tc := range []struct{ model, provider, wantProvider string }{
		{"auto", "", ""}, {"AUTO", "qa", "qa"},
	} {
		o := foldAuto(RunOptions{Model: tc.model, Provider: tc.provider})
		if o.Model != "" || !o.AutoModel || o.Provider != tc.wantProvider {
			t.Errorf("foldAuto(%q) = model %q auto %v provider %q, want untyped auto on %q", tc.model, o.Model, o.AutoModel, o.Provider, tc.wantProvider)
		}
	}
	if o := foldAuto(RunOptions{Model: "claude-opus-5-5"}); o.AutoModel || o.Model != "claude-opus-5-5" {
		t.Errorf("a concrete model folded to %+v", o)
	}
}

// Under auto a hop keeps its cell's row but not its model; an alias cell stays the classifier's alias.
func TestDispatchRequestAutoCell(t *testing.T) {
	setConfigDir(t)
	vaulttest.Use(t)
	d := Dispatch{Agent: labelEngine{stubEngine{name: "opencode"}}, Model: "databricks-claude-sonnet-5-5", Route: route.Route{Provider: "databricks", Profile: "qa"}}
	brief := deepBrief
	if got := dispatchRequest(RunOptions{Prompt: brief, AutoModel: true}, d, true); got.Model != AutoModel || got.CellRoute != d.Route || got.Effort != "high" {
		t.Errorf("auto hop = model %q on %+v effort %q, want auto on qa at the deep tier's effort", got.Model, got.CellRoute, got.Effort)
	}
	if got := dispatchRequest(RunOptions{Prompt: brief, AutoModel: true, Effort: "low"}, d, true); got.Effort != "low" {
		t.Errorf("typed effort became %q under auto", got.Effort)
	}
	if got := dispatchRequest(RunOptions{Prompt: brief}, d, true); got.Model != d.Model || got.Effort != "" {
		t.Errorf("untyped hop = %q effort %q, want the cell as is", got.Model, got.Effort)
	}
	d.Model, d.Route = "sonnet", route.Route{}
	if got := dispatchRequest(RunOptions{Prompt: brief, AutoModel: true}, d, true); got.Model != "sonnet" || !got.CellModel {
		t.Errorf("auto alias cell = %+v, want the bare alias cell", got)
	}
}

// The run's routedModel picks inside the tier band under a typed auto even with a pin, and a pinned auto does too; a concrete pin runs.
func TestRoutedModelAuto(t *testing.T) {
	reg, _, _ := autoFixture(t)
	deep := band(devListing, "deep", false, nil)
	run := func(req RunOptions) string {
		t.Helper()
		got, _, err := SchemaFor("opencode").routedModel(context.Background(), req, providers.Target{Provider: reg.Providers["dev"]}, false)
		if err != nil {
			t.Fatalf("routedModel(%+v): %v", req, err)
		}
		return got.Model
	}
	pin := "databricks-claude-haiku-5-5"
	vaulttest.SetRecord(t, "opencode", "model", pin)
	if got := run(RunOptions{Prompt: deepBrief, AutoModel: true}); !slices.Contains(deep, got) {
		t.Errorf("typed auto over the pin ran %s, want one of %v", got, deep)
	}
	if got := run(RunOptions{Prompt: deepBrief}); got != pin {
		t.Errorf("the pin with nothing typed ran %s, want %s", got, pin)
	}
	vaulttest.SetRecord(t, "opencode", "model", "auto")
	if got := run(RunOptions{Prompt: deepBrief}); !slices.Contains(deep, got) {
		t.Errorf("a pinned auto ran %s, want one of %v", got, deep)
	}
	if f := FreshModelFinding("opencode", reg.Providers["dev"], AutoModel, time.Now()); f != nil {
		t.Errorf("preflight refused auto: %v", f)
	}
}

// A local claude dispatch under a pinned auto runs the tier, not the pin.
func TestClaudeTierPinnedAuto(t *testing.T) {
	setConfigDir(t)
	vaulttest.Use(t)
	vaulttest.SetRecord(t, "claude", "model", "auto")
	d := Dispatch{Agent: labelEngine{stubEngine{name: "claude"}}, keepCaller: true, unroutedDispatch: true}
	if got := dispatchRequest(RunOptions{Prompt: fastBrief}, d, true); got.Model != "haiku" || got.Effort != "low" {
		t.Errorf("pinned-auto claude = model %q effort %q, want haiku at low", got.Model, got.Effort)
	}
}

// The label is the model that runs: a typed or pinned auto rotates over a concrete ladder cell, on the cell's row.
func TestAutoLabelIsRunModel(t *testing.T) {
	reg, _, _ := autoFixture(t)
	ctx := context.Background()
	cell := "databricks-claude-opus-5-5"
	for _, agentName := range []string{"opencode", "codex"} {
		vaulttest.SetRecord(t, agentName, "profile", "dev")
		for _, tc := range []struct {
			name, pin, cell string
			typed           bool
		}{
			{"pinned auto x concrete cell", "auto", cell, false},
			{"typed auto x concrete cell", "", cell, true},
			{"pinned auto x alias cell", "auto", "opus", false},
		} {
			vaulttest.SetRecord(t, agentName, "model", tc.pin)
			for range 12 { // stamp the cell as just picked so the rotation's next pick is another model
				if m, _, _ := RotationModel(reg, agentName, "", true, time.Now()); m == cell {
					break
				}
			}
			d := Dispatch{Agent: labelEngine{stubEngine{name: agentName}}, Model: tc.cell, Account: "databricks·dev"}
			if tc.cell == cell {
				d.Route = rowRoute(reg, "dev")
			}
			q := ModelQuery{Agent: agentName, Task: standardBrief}
			if tc.typed {
				q.Model = AutoModel
			}
			hop, _, err := labelHop(reg, q, agentModelPinContext(ctx, agentName), d)
			label, lerr := ResolveRun(reg, QueryFor(PeekContext(ctx), hop), time.Now())
			lreq := dispatchRequest(RunOptions{Agent: agentName, Prompt: standardBrief, AutoModel: tc.typed}, d, true)
			run, _, _, rerr := SchemaFor(agentName).targetNote(lreq)
			if err = cmp.Or(err, lerr, rerr); err != nil {
				t.Fatalf("%s %s: %v", agentName, tc.name, err)
			}
			if label.Model != run.Model || run.Model == cell {
				t.Errorf("%s %s: label %s, run %s; want the same rotation pick, not the cell's", agentName, tc.name, label.Model, run.Model)
			}
		}
	}
}

// Asked auto on a fresh listing with nothing runnable is ErrPoolExhausted; claude has no pool and steps off its unserved tier.
func TestAutoNoCandidateIsRefused(t *testing.T) {
	reg, _, _ := autoFixture(t)
	saveListing(t, "dev", []string{"databricks-gpt-6-1-sol"}) // Responses-only: neither gemini nor claude ranks it
	for _, fb := range []string{"", router.FallbackNone} {
		q := ModelQuery{Agent: "gemini", Model: AutoModel, Profile: "dev", Task: fastBrief, Fallback: fb}
		if r, err := ResolveRun(reg, q, time.Now()); !errors.Is(err, ErrPoolExhausted) {
			t.Errorf("gemini auto, fallback %q: %s %q %v; want ErrPoolExhausted", fb, r.Model, r.Note, err)
		}
		q.Agent = "claude"
		if r, err := ResolveRun(reg, q, time.Now()); err != nil || r.Model != "databricks-claude-opus-5-5" || !strings.Contains(r.Note, "tier haiku is not served") {
			t.Errorf("claude auto, fallback %q: %s %q %v; want the untiered model, saying the fast tier is not served", fb, r.Model, r.Note, err)
		}
	}
	q := ModelQuery{Agent: "claude", Profile: "dev", Model: "claude-opus-5-5", Fallback: router.FallbackNone}
	if _, err := ResolveRun(reg, q, time.Now()); !errors.Is(err, ErrModelRoute) {
		t.Errorf("a typed concrete unserved model under fallback none ran (%v); want it refused", err)
	}
}

// An untyped run with no fresh listing takes the row's tier model and says so; a fresh listing ranks quietly.
func TestImplicitAutoStaleListingNote(t *testing.T) {
	reg, _, _ := autoFixture(t)
	r, err := ResolveRun(reg, ModelQuery{Agent: "opencode", Profile: "prod", Task: standardBrief}, time.Now())
	if err != nil || !strings.Contains(r.Note, "no fresh model listing on prod, ran the row's sonnet model") {
		t.Errorf("stale untyped run = %s %q %v; want the tier model with a note", r.Model, r.Note, err)
	}
	r, err = ResolveRun(reg, ModelQuery{Agent: "opencode", Profile: "dev", Task: standardBrief}, time.Now())
	if want := oracle(devListing, "standard", false, nil); err != nil || r.Model != want || r.Note != "" {
		t.Errorf("fresh untyped run = %s %q %v; want %s with no note", r.Model, r.Note, err, want)
	}
}

func TestResolveRunOwnConnector(t *testing.T) {
	setConfigDir(t)
	reg := routeRegistry(t)
	for _, model := range []string{"opencode-go/kimi-k3", "opencode/kimi-k3"} {
		q := ModelQuery{Agent: "opencode", Model: model, PinProfile: "dev", PinModel: "databricks-claude-sonnet-5-5"}
		hop, _, err := RunQuery(context.Background(), reg, q, "opus")
		if err != nil {
			t.Fatal(err)
		}
		r, err := ResolveRun(reg, hop, time.Now())
		if err != nil || r.Label() != model {
			t.Fatalf("label=%s err=%v want=%s", r.Label(), err, model)
		}
		id, _, _ := strings.Cut(model, "/")
		if r.Account != id || r.Source != "own-login" || r.Dialect != "own-login" {
			t.Errorf("%s route = account %q source %q dialect %q, want %s on its own login",
				model, r.Account, r.Source, r.Dialect, id)
		}
	}
}

// An own-plan model with an explicit --profile is refused at the run, so the label never reports it as served.
func TestExplainRunOwnConnectorExplicitProfile(t *testing.T) {
	setConfigDir(t)
	reg := routeRegistry(t)
	q := ModelQuery{Agent: "opencode", Model: "opencode-go/kimi-k3", Profile: "ultra"}
	if r, _, err := ExplainRun(reg, q, time.Now()); err == nil {
		t.Errorf("ExplainRun(--profile ultra, %s) = %s, want the run's refusal", q.Model, r.Label())
	}
}

func BenchmarkResolveRunAuto(b *testing.B) {
	reg, _, _ := autoFixture(b)
	time.Sleep(2100 * time.Millisecond) // let every state file settle past the stamp cache's racy window
	q := ModelQuery{Agent: "opencode", Profile: "dev", Model: "auto", Task: deepBrief}
	now := time.Now()
	if _, err := ResolveRun(reg, q, now); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ResolveRun(reg, q, now); err != nil {
			b.Fatal(err)
		}
	}
}

// A menu edit between two resolutions reaches the second: the menu is read once per resolution, never kept.
func TestResolveRunSeesMenuChangeBetweenRuns(t *testing.T) {
	reg, _, _ := autoFixture(t)
	rankedOnly(t)
	q := ModelQuery{Agent: "codex", Profile: "dev", Task: deepBrief}
	if err := providers.SetOpencodeMenu([]providers.OpencodeModel{{ID: "gpt-6-1-sol", Label: "GPT-6.1 Sol"}}); err != nil {
		t.Fatal(err)
	}
	_, first, _ := ExplainRun(reg, q, time.Now())
	if err := providers.SetOpencodeMenu(nil); err != nil {
		t.Fatal(err)
	}
	_, second, err := ExplainRun(reg, q, time.Now())
	if err != nil || second == nil || second.Candidates != 1 || first != nil && first.Candidates != 0 {
		t.Errorf("candidates %+v then %+v (%v); want none, then the one Responses model", first, second, err)
	}
}
