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
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
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
		router.SkipDiscovery(context.Background(), "opencode", rows[i], providers.SpellPin(rows[i], first))
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
	if router.SkipDiscovery(context.Background(), "opencode", rows[0], providers.SpellPin(rows[0], pool[0])) == "" {
		t.Fatalf("fixture: %s not in %s's rotation pool", pool[0], rows[0].Name)
	}
	_, note, err = rotationModelOn(context.Background(), rows, "opencode", models.Sonnet, false, false, nil, time.Now())
	if err != nil || !strings.Contains(note, fmt.Sprintf("%d in pool", len(pool))) {
		t.Errorf("after a dev skip, note %q (%v), want still %d in pool (prod serves it)", note, err, len(pool))
	}
	router.SkipDiscovery(context.Background(), "opencode", rows[1], providers.SpellPin(rows[1], pool[0]))
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
	prodOpus, _ := ResolveModel(reg, ModelQuery{Agent: "opencode", Profile: "prod", Model: "opus"}, now)
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
				router.SkipDiscovery(context.Background(), "opencode", dev, providers.SpellPin(dev, id))
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
		want, err := ResolveModel(reg, ModelQuery{Agent: "claude", Profile: "dev", Model: tc.alias}, now)
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
	if _, held := loadState()[disp.Account].ModelDead[router.ModelKey(m)]; held {
		t.Fatal("a typed --timeout held the model")
	}
	rec := Record{ID: "typed", Agent: "codex", Request: RunOptions{Model: "m", Prompt: "a", TimeoutTyped: true}}
	rec.Finish(wire.Result{}, cut, 0)
	if err := rec.NoteCutoff(now); err != nil || len(readCutoffs()) != 0 {
		t.Fatalf("typed deadline filed a cutoff: %v %+v", err, readCutoffs())
	}
	noteSlowModel(disp, RunOptions{AutoModel: true}, m, cut)
	if _, held := loadState()[disp.Account].ModelDead[router.ModelKey(m)]; !held {
		t.Error("a default-deadline silent auto pick was not held")
	}
	t.Run("typed model idle cutoff", func(t *testing.T) {
		reg, _, _ := autoFixture(t)
		m, _, err := rotationModelOn(context.Background(), []*providers.Provider{reg.Providers["dev"]}, "opencode", models.Sonnet, false, false, nil, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		noteSlowModel(disp, RunOptions{ModelTyped: true}, m, fmt.Errorf("silent: %w", ErrSilent))
		if _, held := loadState()[disp.Account].ModelDead[router.ModelKey(m)]; !held {
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
