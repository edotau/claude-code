package router

import (
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

// TestLoadEveryCellResolves exercises Load() directly (no registry overlay, an isolated empty vault):
// every agent × alias cell has a model, account and billing, and AliasOf round-trips each cell's own
// model back to its alias.
func TestLoadEveryCellResolves(t *testing.T) {
	vaulttest.Use(t)
	tb, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	assertEveryCellResolves(t, tb)
}

// TestLoadFromOverlayCellAndWeights checks LoadFrom applies a registry's per-cell Route override and
// replaces Weights whole, and rejects an overlay cell naming an unknown agent/alias.
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
	bad := &providers.Registry{Routing: providers.Routing{Cells: map[string]map[string]providers.RouteCell{"ghost": {"opus": {Model: "m"}}}}}
	if _, err := LoadFrom(bad, "u"); err == nil {
		t.Error("unknown overlay cell must fail")
	}
}

// TestLoadFromOpencodeOverrideAccountKey checks moving an opencode cell to a different backend derives the
// right billing account instead of keeping the table's opencode:go key (the HIGH quota-attribution finding).
// A github-copilot override bills opencode's own Copilot login (opencode:github-copilot), not the pinned gh
// user's copilot:<user> key — opencode authenticates independently, so conflating the two would misattribute
// quota to an account that never made the call.
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

// TestLoadFromNonOpencodeOverrideWithSlashKeepsAccount checks the account re-derivation guard is scoped to
// the opencode agent: a "/"-bearing model override on another agent (codex here) must not trigger
// opencodeOverrideAccount at all, and the cell keeps the table's own billing account unchanged.
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

// TestTierOfSlotNamesAndFamilies checks AliasOf's other paths: a bare alias name, a provider:model id, and an
// unknown model (none of the table's cells, no alias name, no family match).
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

// TestRowsKeyedByClaudeID checks rows are keyed by canonical Claude id, aliases and spellings reach the same row,
// and the alias view agrees with it.
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

// TestOverlayAcceptsCanonicalAndTierKeys checks routing.cells keyed by canonical id and by the deprecated alias hit
// the same cell.
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
	for _, tc := range []struct{ pin, want string }{{"personal", "personal"}, {"", ""}, {"bare", "bare"}} {
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
		if w := models.DatabricksPrefix + id; w != skip && (runs == nil || runs(w)) {
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
			ready[models.DatabricksPrefix+id] = "READY"
		}
	}
	first := models.DatabricksPrefix + databricksTiers()["opus"][0]
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
