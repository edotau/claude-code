package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/copilotauth"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Asked auto peeks without recording, stamps and advances on a real pick.
func TestRotationAskedAuto(t *testing.T) {
	reg, _, _ := autoFixture(t)
	now := time.Now()
	resolve := func(ctx context.Context) ModelRoute {
		t.Helper()
		r, err := ResolveRun(reg, ModelQuery{Agent: "opencode", Profile: "dev", Model: "auto", Task: standardBrief, ctx: ctx}, now)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	peek := PeekContext(context.Background())
	before := router.LoadState()
	first, again := resolve(peek), resolve(peek)
	if first.Model != again.Model || !reflect.DeepEqual(router.LoadState(), before) {
		t.Fatalf("peeks ran %s then %s, or recorded state", first.Model, again.Model)
	}
	if got := resolve(context.Background()); got.Model != first.Model {
		t.Fatalf("stamping pick ran %s, want the peeked %s", got.Model, first.Model)
	}
	if next := resolve(peek); next.Model == first.Model {
		t.Errorf("peek after a stamp still returned %s, want another pool member", next.Model)
	}
}

// Under the pool pin a workspace's skip drops a model only once no other row serves it unskipped.
func TestRotationPoolPinSkips(t *testing.T) {
	reg, _, _ := autoFixture(t)
	saveListing(t, "prod", devListing)
	rows := []*providers.Provider{reg.Providers["dev"], reg.Providers["prod"]}
	picks := func() []string { // stamped picks cycle the whole pool
		var out []string
		for range rotationPool("opencode", models.Sonnet, rows[0], false, time.Now()) {
			m, _, err := rotationModelOn(context.Background(), rows, "opencode", models.Sonnet, true, false, nil, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, m)
		}
		return out
	}
	first := picks()[0]
	for i, want := range []bool{true, false} {
		router.SkipDiscovery(context.Background(), "opencode", poolOf("opencode", rows[i]), rows[i], providers.SpellPin(rows[i], first))
		if got := picks(); slices.Contains(got, first) != want {
			t.Errorf("after skipping on %d row(s): picks %v, want %s in them %v", i+1, got, first, want)
		}
	}
}

// The pool-pin note's "in pool" count drops a model only once every workspace holds it; one row's hold leaves it.
func TestRotationPoolPinNoteCountsWorkspaceSkips(t *testing.T) {
	reg, _, _ := autoFixture(t)
	saveListing(t, "prod", devListing)
	rows := []*providers.Provider{reg.Providers["dev"], reg.Providers["prod"]}
	pool := rotationPool("opencode", models.Sonnet, rows[0], false, time.Now())
	if len(pool) < 2 {
		t.Fatalf("fixture: standard pool has %d members, want 2+", len(pool))
	}
	_, note, err := rotationModelOn(context.Background(), rows, "opencode", models.Sonnet, false, false, nil, time.Now())
	if err != nil || !strings.Contains(note, fmt.Sprintf("%d in pool", len(pool))) {
		t.Fatalf("note %q (%v), want %d in pool", note, err, len(pool))
	}
	if router.SkipDiscovery(context.Background(), "opencode", poolOf("opencode", rows[0]), rows[0], providers.SpellPin(rows[0], pool[0])) == "" {
		t.Fatalf("fixture: %s not in %s's rotation pool", pool[0], rows[0].Name)
	}
	_, note, err = rotationModelOn(context.Background(), rows, "opencode", models.Sonnet, false, false, nil, time.Now())
	if err != nil || !strings.Contains(note, fmt.Sprintf("%d in pool", len(pool))) {
		t.Errorf("after a dev skip, note %q (%v), want still %d in pool (prod serves it)", note, err, len(pool))
	}
	router.SkipDiscovery(context.Background(), "opencode", poolOf("opencode", rows[1]), rows[1], providers.SpellPin(rows[1], pool[0]))
	_, note, err = rotationModelOn(context.Background(), rows, "opencode", models.Sonnet, false, false, nil, time.Now())
	if err != nil || !strings.Contains(note, fmt.Sprintf("%d in pool", len(pool)-1)) {
		t.Errorf("after the same skip on prod, note %q (%v), want %d in pool", note, err, len(pool)-1)
	}
}

// Stamped picks walk the pool least-recent first: each differs, and after a cycle the first pick returns; dear ones sit out.
func TestRotationLeastRecentCycle(t *testing.T) {
	reg, _, _ := autoFixture(t)
	rows, now := []*providers.Provider{reg.Providers["dev"]}, time.Now()
	pool := rotationPool("opencode", models.Sonnet, rows[0], false, now)
	n := len(pool) - len(catalog.DeferTurns(pool, "standard"))
	if n < 2 {
		t.Fatalf("fixture: standard pool has %d cheaper members, want 2+", n)
	}
	var seen []string
	for range n + 1 {
		m, _, err := rotationModelOn(context.Background(), rows, "opencode", models.Sonnet, true, false, nil, now)
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, m)
	}
	if len(slices.Compact(slices.Sorted(slices.Values(seen[:n])))) != n || seen[n] != seen[0] {
		t.Errorf("picks %v, want %d distinct then the first again", seen, n)
	}
}

func rotationRan(oneOf []string, note string) func(ModelRoute, error) bool {
	return func(r ModelRoute, err error) bool {
		return err == nil && slices.Contains(oneOf, r.Model) && strings.Contains(r.Note, note)
	}
}

// A cold listing runs the tier model, a tier with nothing steps to standard, a fully held tier or unwritable state refuses.
func TestRotationPickOutcomes(t *testing.T) {
	reg, _, pair := autoFixture(t)
	now, dev := time.Now(), reg.Providers["dev"]
	prodOpus, _ := resolveQ(reg, ModelQuery{Agent: "opencode", Profile: "prod", Model: "opus"}, now)
	if len(band(pair, "deep", false, nil)) != 0 || len(band(pair, "standard", false, nil)) == 0 {
		t.Fatalf("fixture: xf must serve standard but not deep, got %v", pair)
	}
	for _, tc := range []struct {
		name, row, model, task string
		setup                  func()
		ok                     func(ModelRoute, error) bool
		writes                 bool
	}{
		{"cold listing", "prod", "auto", deepBrief, func() {}, rotationRan([]string{prodOpus.Model}, "no fresh model listing"), false},
		{"empty tier steps to standard", "xf", "auto", deepBrief, func() {}, rotationRan(band(pair, "standard", false, nil), "(standard tier,"), true},
		{"every tier member held", "dev", "auto", deepBrief, func() {
			for _, id := range rotationPool("opencode", models.Opus, dev, false, now) {
				router.SkipDiscovery(context.Background(), "opencode", poolOf("opencode", dev), dev, providers.SpellPin(dev, id))
			}
		}, func(_ ModelRoute, err error) bool { return errors.Is(err, ErrPoolExhausted) }, true},
		{"state unwritable", "dev", "auto", standardBrief, func() {
			_ = os.Remove(router.StatePath()) // a directory in quota.json's place makes every state write fail
			if err := os.MkdirAll(router.StatePath(), 0o755); err != nil {
				t.Fatal(err)
			}
		}, func(_ ModelRoute, err error) bool {
			return err != nil && strings.Contains(err.Error(), "auto pick state")
		}, false},
	} {
		tc.setup()
		before := router.LoadState()
		r, err := ResolveRun(reg, ModelQuery{Agent: "opencode", Profile: tc.row, Model: tc.model, Task: tc.task}, now)
		if !tc.ok(r, err) {
			t.Errorf("%s: ran %q note %q (%v)", tc.name, r.Model, r.Note, err)
		}
		if !tc.writes && !reflect.DeepEqual(router.LoadState(), before) {
			t.Errorf("%s: rotation state changed", tc.name)
		}
	}
}

// Claude's model never rotates: auto runs the brief's tier model on its row, sonnet for a brief with no tier.
func TestRotationClaudeKeepsTier(t *testing.T) {
	reg, _, _ := autoFixture(t)
	now := time.Now()
	for _, tc := range []struct{ task, alias string }{
		{fastBrief, models.Haiku}, {standardBrief, models.Sonnet}, {deepBrief, models.Opus}, {"", models.Sonnet},
	} {
		want, err := resolveQ(reg, ModelQuery{Agent: "claude", Profile: "dev", Model: tc.alias}, now)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ResolveRun(reg, ModelQuery{Agent: "claude", Profile: "dev", Model: "auto", Task: tc.task}, now)
		if err != nil || got.Model != want.Model {
			t.Errorf("claude auto, brief %q: ran %s (%v), want %s", tc.task, got.Model, err, want.Model)
		}
	}
}

// A deadline or idle cutoff keeps its pool model out of the next auto pick; a stall-free error and the account stay.
func TestNoteSlowModelHoldsPoolModel(t *testing.T) {
	reg, _, _ := autoFixture(t)
	dev, now := reg.Providers["dev"], time.Now()
	pick := func() string {
		t.Helper()
		m, _, err := rotationModelOn(context.Background(), []*providers.Provider{dev}, "opencode", models.Sonnet, false, false, nil, now)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	slow := pick()
	disp := Dispatch{Agent: &fakeRunner{name: "opencode"}, Account: router.ProviderAccount("dev")}
	noteSlowModel(disp, RunOptions{}, slow, errors.New("boom"))
	if pick() != slow {
		t.Fatal("a non-cutoff error held the model")
	}
	noteSlowModel(disp, RunOptions{}, slow, ErrSilent)
	if got := pick(); got == slow {
		t.Errorf("auto pick after an idle cutoff = %s, want another model", got)
	}
	if e := loadState()[disp.Account]; !e.ExhaustedUntil.IsZero() {
		t.Errorf("account benched for a slow model: %+v", e)
	}
}

// A typed-timeout deadline holds nothing and files no cutoff; a default-deadline silent auto pick is held.
func TestNoteSlowModelDeadlineTypedTimeout(t *testing.T) {
	reg, _, _ := autoFixture(t)
	dev, now := reg.Providers["dev"], time.Now()
	m, _, err := rotationModelOn(context.Background(), []*providers.Provider{dev}, "opencode", models.Sonnet, false, false, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	disp := Dispatch{Agent: &fakeRunner{name: "opencode"}, Account: router.ProviderAccount("dev")}
	cut := fmt.Errorf("timed out after 2s: %w", context.DeadlineExceeded)
	noteSlowModel(disp, RunOptions{AutoModel: true, TimeoutTyped: true}, m, cut)
	if _, held := loadState()[disp.Account].ModelDead[models.ModelKey(m)]; held {
		t.Fatal("a typed --timeout held the model")
	}
	rec := Record{ID: "typed", Agent: "codex", Request: RunOptions{Model: "m", Prompt: "a", TimeoutTyped: true}}
	rec.Finish(wire.Result{}, cut, 0)
	if err := rec.NoteCutoff(now); err != nil || len(readCutoffs()) != 0 {
		t.Fatalf("typed deadline filed a cutoff: %v %+v", err, readCutoffs())
	}
	noteSlowModel(disp, RunOptions{AutoModel: true}, m, cut)
	if _, held := loadState()[disp.Account].ModelDead[models.ModelKey(m)]; !held {
		t.Error("a default-deadline silent auto pick was not held")
	}
	t.Run("typed model idle cutoff", func(t *testing.T) {
		reg, _, _ := autoFixture(t)
		m, _, err := rotationModelOn(context.Background(), []*providers.Provider{reg.Providers["dev"]}, "opencode", models.Sonnet, false, false, nil, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		noteSlowModel(disp, RunOptions{ModelTyped: true}, m, fmt.Errorf("silent: %w", ErrSilent))
		if _, held := loadState()[disp.Account].ModelDead[models.ModelKey(m)]; !held {
			t.Error("a typed model's idle cutoff was not held")
		}
	})
}

// A pre-launch "not on the menu" refusal re-picks an auto ask and never swaps a typed model.
func TestRetryModelMenuRefusalRepicksAutoOnly(t *testing.T) {
	reg, _, _ := autoFixture(t)
	row := reg.Row("dev")
	ran := providers.SpellPin(row, rotationPool("opencode", "", row, false, time.Now())[0])
	disp := Dispatch{Agent: &pickRefuser{}, Account: row.Account()}
	msg := errors.New(`pi: model "` + ran + `" is not on the dev menu`)
	if got := retryModel(context.Background(), disp, RunOptions{Model: AutoModel}, ran, msg); got != AutoModel {
		t.Fatalf("auto retry = %q, want the auto re-pick", got)
	}
	if got := retryModel(context.Background(), disp, RunOptions{Model: ran}, ran, msg); got != "" {
		t.Fatalf("typed retry = %q, want none", got)
	}
}

// An agent driver's Runnable narrows auto's pool to models it can run.
func TestDriverRunnableFiltersPool(t *testing.T) {
	registryMu.Lock()
	drivers["pi"] = onlyKeep{NewBaseEngine("pi"), "keep"}
	registryMu.Unlock()
	t.Cleanup(func() { registryMu.Lock(); delete(drivers, "pi"); registryMu.Unlock() })
	got := driverRunnable("pi", &providers.Provider{}, []catalog.Candidate{{Model: "keep"}, {Model: "drop"}})
	if len(got) != 1 || got[0].Model != "keep" {
		t.Fatalf("pool = %v, want only keep", got)
	}
}

type onlyKeep struct {
	BaseEngine
	id string
}

func (o onlyKeep) Runnable(_ *providers.Provider, m string) bool { return m == o.id }

// Wording supplies an absent tier; explicit tiers override it and restrict every ready model.
func TestDispatchRoleTierModels(t *testing.T) {
	for _, tc := range []struct {
		name, prompt string
		role         router.Role
		model        string
	}{
		{"untiered audit is deep opus", "audit the security of the auth parser", router.RoleOwner, models.Opus},
		{"standard is sonnet despite audit wording", "Tier: standard\naudit security", router.RoleFleet, models.Sonnet},
		{"fast is haiku despite audit wording", "Tier: fast\naudit security", router.RoleFleet, models.Haiku},
		{"explicit deep is opus", "Tier: deep\ndo it", router.RoleOwner, models.Opus},
		{"untiered standard is sonnet", "Implement a settings panel", router.RoleFleet, models.Sonnet},
		{"untiered fast is haiku", "Fix a typo in README.md", router.RoleFleet, models.Haiku},
		{"empty defaults to sonnet", "", router.RoleFleet, models.Sonnet},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dispatchRole(context.Background(), tc.prompt); got != tc.role {
				t.Errorf("dispatchRole(%q) = %s, want %s", tc.prompt, got, tc.role)
			}
			for _, mode := range []struct {
				name, session string
				exclusive     bool
				missingTier   bool
				providers     []string
			}{
				{"rotating", "dev", false, false, []string{"anthropic", "databricks-dev", "databricks-qa"}},
				{"exclusive elsewhere", "dev", true, false, []string{"databricks-dev", "databricks-qa"}},
				{"exclusive start", "ultra", true, false, []string{"anthropic"}},
				{"exclusive start lacks tier", "ultra", true, true, nil},
			} {
				t.Run(mode.name, func(t *testing.T) {
					checkDispatchTierCells(t, tc.prompt, tc.model, mode.session, mode.exclusive, mode.missingTier, mode.providers)
				})
			}
		})
	}
}

func checkDispatchTierCells(t *testing.T, prompt, model, session string, exclusive, missingTier bool, want []string) {
	t.Helper()
	tiers := func() map[string]string {
		return map[string]string{"opus": "claude-opus-5-5", "sonnet": "claude-sonnet-5-5", "haiku": "claude-haiku-5-5"}
	}
	reg := &providers.Registry{Providers: map[string]*providers.Provider{
		"ultra": {Name: "ultra", Provider: "anthropic", Models: tiers()},
		"dev":   {Name: "dev", Provider: "databricks", Preset: providers.PresetDatabricks, Models: tiers()},
		"qa":    {Name: "qa", Provider: "databricks", Preset: providers.PresetDatabricks, Models: tiers()},
	}}
	if missingTier {
		delete(reg.Providers["ultra"].Models, model)
	}
	modes := map[string]string{}
	if exclusive {
		modes["anthropic"] = "exclusive"
	}
	cells, scope := dispatchCells(context.Background(), reg, modes, prompt, session)
	var got []string
	for _, c := range cells {
		if c.Model != model {
			t.Errorf("model = %s, want %s", c.Model, model)
		}
		got = append(got, c.Provider)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("providers = %v, want %v", got, want)
	}
	if missingTier {
		o, err := noReadyCell(reg, RunOptions{}, session, scope)
		if err != nil || o.Provider != session {
			t.Errorf("empty exclusive tier: provider %q, err %v; want %s", o.Provider, err, session)
		}
	}
}

// An exclusive provider keeps its own starts and takes no one else's.
func TestRoleRotationModes(t *testing.T) {
	tiers := func(prefix string) map[string]string {
		return map[string]string{"opus": prefix + "claude-opus-5-5", "sonnet": prefix + "claude-sonnet-5-5", "haiku": prefix + "claude-haiku-5-5"}
	}
	reg := &providers.Registry{Providers: map[string]*providers.Provider{
		"ultra": {Name: "ultra", Provider: "anthropic", Models: tiers("")},
		"dev":   {Name: "dev", Provider: "databricks", Preset: providers.PresetDatabricks, Models: tiers("databricks-")},
		"qa":    {Name: "qa", Provider: "databricks", Preset: providers.PresetDatabricks, Models: tiers("databricks-")},
	}}
	excl := map[string]string{"anthropic": "exclusive"}
	if cells, _ := modeCells(reg, nil, router.RoleOwner, "dev"); len(cells) != 3 {
		t.Fatalf("rotate: owner cells = %v, want 3", cells)
	}
	if cells, scope := modeCells(reg, excl, router.RoleFleet, "dev"); len(cells) != 4 || slices.ContainsFunc(cells,
		func(c router.RoleCell) bool { return c.Provider == "anthropic" }) || !strings.Contains(scope.String(), "kept out") {
		t.Fatalf("exclusive elsewhere: cells %v scope %q", cells, scope)
	}
	if cells, scope := modeCells(reg, excl, router.RoleFleet, "ultra"); len(cells) != 2 || cells[0].Provider != "anthropic" ||
		!strings.Contains(scope.String(), "stays on") {
		t.Fatalf("exclusive start: cells %v scope %q", cells, scope)
	}
}

// An arm-confined (databricks:<p>) or provider-fenced dispatch does not rotate at all; an unfenced one may.
func TestRoleRotationFenced(t *testing.T) {
	o := RunOptions{Agent: "claude", Prompt: "say hi"}
	if ok, _ := untypedClaude(context.Background(), o); !ok {
		t.Fatal("an unfenced untyped claude dispatch should rotate")
	}
	for _, fence := range []string{`{"provider":"databricks","profile":"qa"}`, `{"provider":"anthropic"}`} {
		ctx := valuesContext(context.Background(), proc.Values{route.OverlayVar: fence})
		if ok, _ := untypedClaude(ctx, o); ok {
			t.Errorf("fence %s: rotated", fence)
		}
	}
}

// A rotation that stays on an exclusive start pins the row as Provider: Lead stays empty and the record marks Rotated.
func TestPlaceRotationStayPinsProvider(t *testing.T) {
	reg := &providers.Registry{Providers: map[string]*providers.Provider{"ultra": {Name: "ultra", Provider: "anthropic"}}}
	_, o := placeRotation(context.Background(), reg, RunOptions{Lead: "old"}, "ultra", modeScope{stay: true, off: []string{"anthropic"}})
	if o.Lead != "" || o.Provider != "ultra" || !o.Rotated {
		t.Fatalf("lead %q provider %q rotated %v, want empty, ultra, true", o.Lead, o.Provider, o.Rotated)
	}
}

func TestRotationCandidatesLastResort(t *testing.T) {
	vaulttest.Use(t)
	copilotauth.StubRead(t, func(string) []string {
		return []string{"auto", "gpt-6.2-sol", "claude-sonnet-5.5"}
	}, nil)
	local := &providers.Provider{
		Name: "local", Provider: "magnitude", Options: providers.Options{LastResort: true},
		Models: map[string]string{
			"haiku": "qwen3.8-27b:gguf:q6", "sonnet": "qwen3.8-27b:gguf:q6", "opus": "qwen3.8-35b",
		},
	}
	copilot := &providers.Provider{
		Name: "copilot", Provider: "github", Models: map[string]string{"sonnet": "not-in-catalog"},
	}
	for _, tt := range []struct {
		name string
		row  *providers.Provider
		want []catalog.Candidate
	}{
		{"local", local, []catalog.Candidate{{Model: "qwen3.8-27b:gguf:q6"}, {Model: "qwen3.8-35b"}}},
		{"copilot", copilot, []catalog.Candidate{{Model: "gpt-6.2-sol"}, {Model: "claude-sonnet-5.5"}}},
		{"empty local", &providers.Provider{Options: providers.Options{LastResort: true}}, nil},
		{"last-resort first", &providers.Provider{
			Preset: providers.PresetDatabricks, Options: providers.Options{LastResort: true}, Models: local.Models,
		}, []catalog.Candidate{{Model: "qwen3.8-27b:gguf:q6"}, {Model: "qwen3.8-35b"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if !router.RotationRow(tt.row) {
				t.Fatal("row not admitted to rotation")
			}
			for _, tools := range []bool{false, true} {
				if got := rotationCandidates(nil, "pi", tt.row, tools, time.Time{}); !reflect.DeepEqual(got, tt.want) {
					t.Fatalf("tools=%v candidates = %v, want %v", tools, got, tt.want)
				}
			}
		})
	}
	for _, a := range []string{"copilot", "opencode", "codex"} {
		if got := rotationCandidates(nil, a, local, false, time.Time{}); got != nil {
			t.Fatalf("%s on the local row = %v, want none", a, got)
		}
	}
	local.Options.LastResort = false
	if router.RotationRow(local) || router.RotationRow(nil) {
		t.Fatal("ordinary or nil row admitted to rotation")
	}
}

// A model on the picker's drop list leaves the rotation candidates; undrop brings it back.
func TestWithoutDeprecatedSkipsDroppedModels(t *testing.T) {
	vaulttest.Use(t)
	p := &providers.Provider{Name: "dev", Provider: "databricks", Preset: providers.PresetDatabricks,
		Host: "https://dev.example.com", Auth: providers.Auth{Type: providers.AuthOAuth}}
	cands := []catalog.Candidate{{Model: "databricks-gpt-5-mini"}, {Model: "databricks-claude-sonnet-4-5"}}
	if _, err := catalog.RecordDropped(p, "databricks-gpt-5-mini", "deprecated", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := withoutDeprecated(p, append([]catalog.Candidate(nil), cands...)); len(got) != 1 || got[0].Model != cands[1].Model {
		t.Fatalf("pool = %v, want only %s", got, cands[1].Model)
	}
	if _, err := catalog.EditPickerDrops(p, []string{"databricks-gpt-5-mini"}, true, true); err != nil {
		t.Fatal(err)
	}
	if got := withoutDeprecated(p, append([]catalog.Candidate(nil), cands...)); len(got) != 2 {
		t.Fatalf("pool after undrop = %v, want both", got)
	}
}
