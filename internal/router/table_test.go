package router

import (
	"cmp"
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Load() with an empty vault gives every agent x alias cell a model, account and billing; AliasOf round-trips.
func TestLoadEveryCellResolves(t *testing.T) {
	vaulttest.Use(t)
	tb, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	assertEveryCellResolves(t, tb)
}

// LoadFrom applies a registry's per-cell Route override, replaces Weights whole, and skips unknown cells.
func TestLoadFromOverlayCellAndWeights(t *testing.T) {
	reg := &providers.Registry{
		// opencode-go/y keeps the opencode cell's own backend prefix, so the override only replaces the model.
		Routing: providers.Routing{
			Cells:   map[string]map[string]providers.RouteCell{"opencode": {"sonnet": {Model: "opencode-go/y"}}},
			Weights: map[string]float64{"price": 0.2, "headroom": 0.8},
		},
	}
	tb, err := LoadFrom(reg, "u")
	if err != nil {
		t.Fatal(err)
	}
	if c := tb.Rows["opencode"]["sonnet"]; c.Model != "opencode-go/y" || c.Effort != "high" || c.Account != "opencode·go" {
		t.Errorf("overlay cell = %+v, want model replaced, effort kept, account unchanged", c)
	}
	if tb.Weights["headroom"] != 0.8 {
		t.Errorf("weights = %v", tb.Weights)
	}
	stale := &providers.Registry{Routing: providers.Routing{Cells: map[string]map[string]providers.RouteCell{
		"ghost": {"opus": {Model: "m"}}, "opencode": {"sonnet": {Model: "opencode-go/y"}},
	}}}
	tb, err = LoadFrom(stale, "u")
	if err != nil {
		t.Fatalf("a stale overlay cell must be skipped, not fail the table: %v", err)
	}
	if c := tb.Rows["opencode"]["sonnet"]; c.Model != "opencode-go/y" {
		t.Errorf("cells beside a stale one must still apply: %+v", c)
	}
}

// Moving an opencode cell to another backend re-derives its billing account, not the gh user's copilot key.
func TestLoadFromOpencodeOverrideAccountKey(t *testing.T) {
	toCopilot := &providers.Registry{
		Routing: providers.Routing{Cells: map[string]map[string]providers.RouteCell{"opencode": {"sonnet": {Model: "github-copilot/kimi-k3"}}}},
	}
	tb, err := LoadFrom(toCopilot, "work-user")
	if err != nil {
		t.Fatal(err)
	}
	if c := tb.Rows["opencode"]["sonnet"]; c.Account != "opencode·github-copilot" {
		t.Errorf("account = %q, want opencode's own copilot login account, not the pinned gh user's", c.Account)
	}
	unknown := &providers.Registry{
		Routing: providers.Routing{Cells: map[string]map[string]providers.RouteCell{"opencode": {"sonnet": {Model: "zed/overridden-model"}}}},
	}
	if _, err := LoadFrom(unknown, "u"); err == nil {
		t.Error("an opencode override to an unknown backend prefix must fail, not silently keep opencode:go billing")
	}
	explicit := &providers.Registry{
		Routing: providers.Routing{Cells: map[string]map[string]providers.RouteCell{
			"opencode": {"sonnet": {Model: "zed/overridden-model", Account: "opencode·go"}},
		}},
	}
	tb, err = LoadFrom(explicit, "u")
	if err != nil {
		t.Fatal(err)
	}
	if c := tb.Rows["opencode"]["sonnet"]; c.Account != "opencode·go" {
		t.Errorf("account = %q, want the explicit override honored", c.Account)
	}
	badAccount := &providers.Registry{
		Routing: providers.Routing{Cells: map[string]map[string]providers.RouteCell{
			"opencode": {"sonnet": {Model: "opencode-go/y", Account: "nobody:nothing"}},
		}},
	}
	if _, err := LoadFrom(badAccount, "u"); err == nil {
		t.Error("an explicit account_key naming no known billing entry must fail")
	}
}

// The account re-derivation guard is opencode-only: a "/" model override on codex keeps its billing account.
func TestLoadFromNonOpencodeOverrideWithSlashKeepsAccount(t *testing.T) {
	reg := &providers.Registry{
		Routing: providers.Routing{Cells: map[string]map[string]providers.RouteCell{"codex": {"sonnet": {Model: "openrouter/some-model"}}}},
	}
	tb, err := LoadFrom(reg, "u")
	if err != nil {
		t.Fatal(err)
	}
	c := tb.Rows["codex"]["sonnet"]
	if c.Model != "openrouter/some-model" {
		t.Errorf("model = %q, want the override applied", c.Model)
	}
	if c.Account != "openai·chatgpt" {
		t.Errorf("account = %q, want the table's own codex account unchanged (no opencode re-derivation off-agent)", c.Account)
	}
}

// TestLoadFromCopilotAccountKey checks the pinned gh user replaces every copilot cell's account key.
func TestLoadFromCopilotAccountKey(t *testing.T) {
	tb, err := LoadFrom(nil, "work-user")
	if err != nil {
		t.Fatal(err)
	}
	if a := tb.Rows["copilot"]["sonnet"].Account; a != "github·work-user" {
		t.Errorf("copilot account = %q, want the pinned user only", a)
	}
	tb, err = LoadFrom(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if a := tb.Rows["copilot"]["sonnet"].Account; a != copilotUnpinned {
		t.Errorf("copilot account = %q, want %q when no user is pinned", a, copilotUnpinned)
	}
}

// AliasOf's other paths: a bare alias name, a provider:model id, and an unknown model.
func TestTierOfSlotNamesAndFamilies(t *testing.T) {
	vaulttest.Use(t)
	tb, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := tb.AliasOf("opus"); got != "opus" {
		t.Errorf("AliasOf(opus) = %q", got)
	}
	if got := tb.AliasOf("claude-opus-5-5[1m]"); got != "opus" {
		t.Errorf("AliasOf(claude id) = %q", got)
	}
	if got := tb.AliasOf(""); got != "" {
		t.Errorf("AliasOf(empty) = %q, want empty", got)
	}
	if got := tb.AliasOf("no-such-model-anywhere"); got != "" {
		t.Errorf("AliasOf(unknown) = %q, want empty", got)
	}
}

// Rows are keyed by canonical Claude id, aliases and spellings reach the same row, and the alias view agrees.
func TestRowsKeyedByClaudeID(t *testing.T) {
	vaulttest.Use(t)
	tb, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range Aliases {
		id, _ := models.Alias(alias)
		for _, key := range []string{alias, id, strings.TrimPrefix(id, "claude-")} {
			row, ok := tb.ByModel[rowKey(key)]
			if !ok || len(row) != len(TableAgents) {
				t.Fatalf("Row(%q) ok=%v runners=%d", key, ok, len(row))
			}
			if row["claude"].Model != id || row["claude"] != tb.Rows["claude"][alias] {
				t.Errorf("Row(%q) claude = %+v, want model %s == alias view", key, row["claude"], id)
			}
		}
	}
	if _, ok := tb.ByModel[rowKey("gpt-6-1-sol")]; ok {
		t.Error("a non-Claude id must not name a row")
	}
}

// routing.cells keyed by canonical id and by the deprecated alias hit the same cell.
func TestOverlayAcceptsCanonicalAndTierKeys(t *testing.T) {
	for _, key := range []string{"sonnet", "claude-sonnet-5-5"} {
		reg := &providers.Registry{Routing: providers.Routing{Cells: map[string]map[string]providers.RouteCell{
			"codex": {key: {Effort: "low"}}}}}
		tb, err := LoadFrom(reg, "u")
		if err != nil {
			t.Fatal(err)
		}
		if tb.Rows["codex"]["sonnet"].Effort != "low" || tb.ByModel["claude-sonnet-5-5"]["codex"].Effort != "low" {
			t.Errorf("key %q: overlay not applied", key)
		}
	}
}

// TestCopilotUserFollowsProfilePin checks the billed copilot account is the pinned github profile's name.
func TestCopilotUserFollowsProfilePin(t *testing.T) {
	row := `"protocol":"openai","base_url":"https://api.githubcopilot.com","auth":{"type":"cli","login":"copilot"},` +
		`"models":{"opus":"gpt-6.1-sol","sonnet":"claude-sonnet-5.5","haiku":"auto"}`
	for _, tc := range []struct{ pin, want string }{{"tester", "tester"}, {"personal", "tester"}, {"", ""}, {"bare", "bare"}} {
		vaulttest.Use(t)
		vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"github":{"personal":{`+row+`,"login":"tester"},"bare":{`+row+`}}}}`)
		if tc.pin != "" {
			vaulttest.SetRecord(t, "copilot", "profile", tc.pin)
		}
		if got := CopilotUser(); got != tc.want {
			t.Errorf("pin %q: CopilotUser = %q, want %q", tc.pin, got, tc.want)
		}
	}
}

// By design: providers keeps an overlay cell whole (no model); the router merges it field by field over table.json.
func TestEffortOnlyOverlayCellHasNoModelInRegistryButKeepsModelInRouter(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"routing":{"cells":{"codex":{"sonnet":{"effort":"low"}}}}}`)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	if c := reg.Routing.Cells["codex"]["sonnet"]; c.Model != "" || c.Effort != "low" {
		t.Errorf("registry cell = %+v, want effort low and no model (the overlay cell replaced whole)", c)
	}
	tb, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c := tb.Rows["codex"]["sonnet"]; c.Model == "" || c.Effort != "low" {
		t.Errorf("router cell = %+v, want effort low and table.json's model kept (merged field by field)", c)
	}
}

func TestAliasOfColonBearingModelIDTriesFullID(t *testing.T) {
	vaulttest.Use(t)
	tb, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := tb.AliasOf("claude-opus-5-5:0"); got != "opus" {
		t.Errorf("AliasOf(claude-opus-5-5:0) = %q, want opus (colon is part of the id)", got)
	}
}

// TestTableColumnsAndEffortsMatchAgentid checks table.json column keys are agentid names with efforts valid for them.
func TestTableColumnsAndEffortsMatchAgentid(t *testing.T) {
	var doc struct {
		Rows map[string]map[string]struct {
			Effort string `json:"effort"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(tableJSON, &doc); err != nil {
		t.Fatal(err)
	}
	for row, cols := range doc.Rows {
		for agent, cell := range cols {
			if !agentid.Known(agent) {
				t.Errorf("row %s: column %q is not an agentid name", row, agent)
				continue
			}
			if err := agentid.ValidEffort(agent, cell.Effort); err != nil {
				t.Errorf("row %s, column %s: %v", row, agent, err)
			}
		}
	}
}

// bucketWant is the first id of alias's bucket agent can run, as wired on Databricks, other than skip; "" if none.
func bucketWant(p *providers.Provider, agent, alias, skip string) string {
	runs := map[string]func(string) bool{
		"claude": func(w string) bool { return models.FamilyKind(w) == models.FamilyClaude },
		"gemini": func(w string) bool { return models.FamilyKind(w) == models.FamilyGemini },
		"codex":  func(w string) bool { return providers.ServesResponses(p, w) },
	}[agent]
	for _, id := range databricksTiers()[alias] {
		if w := models.DatabricksPrefix + canonicalID(cmp.Or(providers.FamilyDefaults[id], id)); w != skip && (runs == nil || runs(w)) {
			return w
		}
	}
	return ""
}

// TestDatabricksModelPicksFirstRunnableBucketMember: each agent gets its bucket's first runnable, READY, unmarked id.
func TestDatabricksModelPicksFirstRunnableBucketMember(t *testing.T) {
	vaulttest.Use(t)
	p, ready := dbxProv("dev"), map[string]string{}
	for _, ids := range databricksTiers() {
		for _, id := range ids {
			ready[models.DatabricksPrefix+canonicalID(cmp.Or(providers.FamilyDefaults[id], id))] = "READY"
		}
	}
	first := models.DatabricksPrefix + canonicalID(cmp.Or(providers.FamilyDefaults[databricksTiers()["opus"][0]], databricksTiers()["opus"][0]))
	notReady := maps.Clone(ready)
	notReady[first] = "NOT_READY"
	for _, c := range []struct {
		name    string
		listing map[string]string
		mark    bool
		skip    string
	}{{"all ready", ready, false, ""}, {"first not ready", notReady, false, first}, {"first unserved", ready, true, first}} {
		if err := SaveServed(listing("dev", c.listing)); err != nil {
			t.Fatal(err)
		}
		if c.mark {
			if err := MarkUnserved(p, first); err != nil {
				t.Fatal(err)
			}
		}
		for _, agent := range TableAgents {
			for _, alias := range Aliases {
				if got, w := DatabricksModel(agent, alias, p), bucketWant(p, agent, alias, c.skip); got != w {
					t.Errorf("%s: DatabricksModel(%s, %s) = %q, want %q", c.name, agent, alias, got, w)
				}
			}
		}
	}
	if got := DatabricksModel("copilot", "opus", &providers.Provider{Name: "gh", Provider: "github"}); got != "" {
		t.Errorf("non-Databricks provider = %q, want \"\"", got)
	}
}

func TestModelRow(t *testing.T) {
	cases := []struct{ in, alias, row, exact string }{
		{"opus", "opus", "claude-opus-5-5", ""},
		{"claude-sonnet-5.5", "sonnet", "claude-sonnet-5-5", ""},
		{"databricks-claude-haiku-5-5", "haiku", "claude-haiku-5-5", ""},
		{"gpt-6.1-sol", "", "", "gpt-6-1-sol"},
		{"glm-5.3", "", "", "glm-5-3"},
		// off-catalog is still an exact request, never a silent default
		{"no-such-model", "", "", "no-such-model"},
		{"", "", "", ""},
	}
	for _, c := range cases {
		alias, row, exact := ModelRow(c.in)
		if alias != c.alias || row != c.row || exact != c.exact {
			t.Errorf("ModelRow(%q) = (%q, %q, %q), want (%q, %q, %q)", c.in, alias, row, exact, c.alias, c.row, c.exact)
		}
	}
}

func TestClaudeCellModelIsTheAlias(t *testing.T) {
	if got := ClaudeCellModel("claude-opus-5-5"); got != "opus" {
		t.Errorf("ClaudeCellModel(claude-opus-5-5) = %q, want opus (so pins resolve it)", got)
	}
	if got := ClaudeCellModel("claude-opus-4-1"); got != "claude-opus-4-1" {
		t.Errorf("an alias-less row stays canonical, got %q", got)
	}
}

// AsksModel matches a databricks- spelling of a non-catalog id; LowestAliasServing a gemini effort suffix.
func TestAsksModelWireSpellingAndGeminiEffortCell(t *testing.T) {
	if !AsksModel("databricks-foo-9", "foo-9") {
		t.Error("AsksModel missed the databricks- spelling of a non-catalog id")
	}
	if AsksModel("foo-9", "foo-8") {
		t.Error("AsksModel matched different models")
	}
	tb := testTable(t)
	if got := tb.LowestAliasServing("gemini", "gemini-3-8-flash"); got != "haiku" {
		t.Errorf("LowestAliasServing(gemini-3-8-flash) = %q, want haiku (cell gemini-3.8-flash-low)", got)
	}
}

// A gemini dispatch at opus on Databricks picks a gemini bucket member, never the Claude opus alias model.

func BenchmarkLoadFrom(b *testing.B) {
	testTable(b)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := LoadFrom(nil, "work-user"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAliasOf(b *testing.B) {
	tb := testTable(b)
	models := []string{"claude-opus-5-5[1m]", "claude-sonnet-5-5", "gpt-5-5", "databricks-claude-haiku-5-5", "unknown-model"}
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		tb.AliasOf(models[i%len(models)])
	}
}

// canonicalID is id as the catalog spells it (dashed), or id itself when the catalog does not list it.
func canonicalID(id string) string {
	if c, _, ok := models.Resolve(id); ok {
		return c
	}
	return id
}
