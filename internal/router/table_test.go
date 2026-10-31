package router

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestLoadEveryCellResolves exercises Load() directly (no registry overlay, an isolated empty vault):
// every runner × tier cell has a model, account and billing, and TierOf round-trips each cell's own
// model back to its tier.
func TestLoadEveryCellResolves(t *testing.T) {
	vaulttest.Use(t)
	tb, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range TableRunners {
		for _, tier := range Tiers {
			c, ok := tb.Cell(r, tier)
			if !ok || c.Model == "" || c.Account == "" || tb.Billing[c.Account] == "" {
				t.Fatalf("%s/%s: cell %+v ok=%v billing=%q", r, tier, c, ok, tb.Billing[c.Account])
			}
			// codex and opencode serve one id at both opus and sonnet: the strongest tier claims a shared id.
			if got := tb.TierOf(c.Model); got != tier && ((r != "codex" && r != "opencode") || tier != "sonnet" || got != "opus") {
				t.Errorf("TierOf(%s) = %q, want %q", c.Model, got, tier)
			}
		}
	}
}

// TestLoadFromOverlayCellAndWeights checks LoadFrom applies a registry's per-cell Route override and
// replaces Weights whole, and rejects an overlay cell naming an unknown runner/tier.
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
// the opencode runner: a "/"-bearing model override on another runner (codex here) must not trigger
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
		t.Errorf("account = %q, want the table's own codex account unchanged (no opencode re-derivation off-runner)", c.Account)
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
	if a := tb.Rows["copilot"]["sonnet"].Account; a != CopilotUnpinned {
		t.Errorf("copilot account = %q, want %q when no user is pinned", a, CopilotUnpinned)
	}
}

// TestTierOfSlotNamesAndFamilies checks TierOf's other paths: a bare slot name, a provider:model id, and an
// unknown model (none of the table's cells, no slot name, no family match).
func TestTierOfSlotNamesAndFamilies(t *testing.T) {
	vaulttest.Use(t)
	tb, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := tb.TierOf("opus"); got != "opus" {
		t.Errorf("TierOf(opus) = %q", got)
	}
	if got := tb.TierOf("claude-opus-5-5[1m]"); got != "opus" {
		t.Errorf("TierOf(claude id) = %q", got)
	}
	if got := tb.TierOf(""); got != "" {
		t.Errorf("TierOf(empty) = %q, want empty", got)
	}
	if got := tb.TierOf("no-such-model-anywhere"); got != "" {
		t.Errorf("TierOf(unknown) = %q, want empty", got)
	}
}

// TestRowsKeyedByClaudeID checks rows are keyed by canonical Claude id, aliases and spellings reach the same row,
// and the tier view agrees with it.
func TestRowsKeyedByClaudeID(t *testing.T) {
	vaulttest.Use(t)
	tb, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, tier := range Tiers {
		id, _ := models.Alias(tier)
		for _, key := range []string{tier, id, strings.TrimPrefix(id, "claude-")} {
			row, ok := tb.Row(key)
			if !ok || len(row) != len(TableRunners) {
				t.Fatalf("Row(%q) ok=%v runners=%d", key, ok, len(row))
			}
			if row["claude"].Model != id || row["claude"] != tb.Rows["claude"][tier] {
				t.Errorf("Row(%q) claude = %+v, want model %s == tier view", key, row["claude"], id)
			}
		}
	}
	if _, ok := tb.Row("gpt-6-1-sol"); ok {
		t.Error("a non-Claude id must not name a row")
	}
}

// TestOverlayAcceptsCanonicalAndTierKeys checks routing.cells keyed by canonical id and by the deprecated tier hit
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
		vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"github":{"personal":{`+row+`,"login":"edotau"},"bare":{`+row+`}}}}`)
		if tc.pin != "" {
			vaulttest.SetRecord(t, "copilot", "profile", tc.pin)
		}
		if got := CopilotUser(); got != tc.want {
			t.Errorf("pin %q: CopilotUser = %q, want %q", tc.pin, got, tc.want)
		}
	}
}
