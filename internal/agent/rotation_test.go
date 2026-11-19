package agent

import (
	"context"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Asked auto peeks without recording, stamps and advances on a real pick.
func TestRotationAskedAuto(t *testing.T) {
	reg, _, _ := autoFixture(t)
	now := time.Now()
	resolve := func(ctx context.Context) ModelRoute {
		t.Helper()
		r, err := ResolveRun(reg, ModelQuery{Agent: "opencode", Model: "dev:auto", Task: standardBrief, ctx: ctx}, now)
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
			m, _, err := rotationModelOn(rows, "opencode", models.Sonnet, true, false, nil, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, m)
		}
		return out
	}
	first := picks()[0]
	for i, want := range []bool{true, false} {
		router.SkipDiscovery("opencode", rows[i], providers.SpellPin(rows[i], first))
		if got := picks(); slices.Contains(got, first) != want {
			t.Errorf("after skipping on %d row(s): picks %v, want %s in them %v", i+1, got, first, want)
		}
	}
}

// Stamped picks walk the pool least-recent first: each differs, and the pool's turn after a full cycle is the first pick.
func TestRotationLeastRecentCycle(t *testing.T) {
	reg, _, _ := autoFixture(t)
	rows, now := []*providers.Provider{reg.Providers["dev"]}, time.Now()
	n := len(rotationPool("opencode", models.Sonnet, rows[0], false, now))
	if n < 2 {
		t.Fatalf("fixture: standard pool has %d members, want 2+", n)
	}
	var seen []string
	for range n + 1 {
		m, _, err := rotationModelOn(rows, "opencode", models.Sonnet, true, false, nil, now)
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
	prodOpus, _ := ResolveModel(reg, ModelQuery{Agent: "opencode", Model: "prod:opus"}, now)
	if len(band(pair, "deep", false, nil)) != 0 || len(band(pair, "standard", false, nil)) == 0 {
		t.Fatalf("fixture: xf must serve standard but not deep, got %v", pair)
	}
	for _, tc := range []struct {
		name, model, task string
		setup             func()
		ok                func(ModelRoute, error) bool
		writes            bool
	}{
		{"cold listing", "prod:auto", deepBrief, func() {}, rotationRan([]string{prodOpus.Model}, "no fresh model listing"), false},
		{"empty tier steps to standard", "xf:auto", deepBrief, func() {}, rotationRan(band(pair, "standard", false, nil), "(standard tier,"), true},
		{"every tier member held", "dev:auto", deepBrief, func() {
			for _, id := range rotationPool("opencode", models.Opus, dev, false, now) {
				router.SkipDiscovery("opencode", dev, providers.SpellPin(dev, id))
			}
		}, func(_ ModelRoute, err error) bool { return errors.Is(err, ErrPoolExhausted) }, true},
		{"state unwritable", "dev:auto", standardBrief, func() {
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
		r, err := ResolveRun(reg, ModelQuery{Agent: "opencode", Model: tc.model, Task: tc.task}, now)
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
		want, err := ResolveModel(reg, ModelQuery{Agent: "claude", Model: "dev:" + tc.alias}, now)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ResolveRun(reg, ModelQuery{Agent: "claude", Model: "dev:auto", Task: tc.task}, now)
		if err != nil || got.Model != want.Model {
			t.Errorf("claude auto, brief %q: ran %s (%v), want %s", tc.task, got.Model, err, want.Model)
		}
	}
}

// Only an unconfined asked auto on claude rotates rows, and the subscription row is the cli login, never the key row.
func TestClaudeRowRotation(t *testing.T) {
	setConfigDir(t)
	vaulttest.Use(t)
	t.Setenv("COWORKER_OVERLAY", "")
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		q    RouteReq
		want bool
	}{
		{"typed auto", RouteReq{Agent: "claude", Model: "auto"}, true},
		{"auto flag", RouteReq{Agent: "claude", AutoModel: true}, true},
		{"--provider", RouteReq{Agent: "claude", Model: "auto", Provider: "databricks"}, false},
		{"--profile", RouteReq{Agent: "claude", Model: "auto", Profile: "qa"}, false},
		{"row prefix", RouteReq{Agent: "claude", Model: "dev:auto"}, false},
		{"typed alias", RouteReq{Agent: "claude", Model: "sonnet"}, false},
		{"typed fable", RouteReq{Agent: "claude", Model: "fable"}, false},
		{"untyped", RouteReq{Agent: "claude"}, false},
		{"other agent", RouteReq{Agent: "codex", Model: "auto"}, false},
	} {
		if got := rowRotates(ctx, tc.q); got != tc.want {
			t.Errorf("%s: rowRotates = %v, want %v", tc.name, got, tc.want)
		}
	}
	reg := &providers.Registry{Providers: map[string]*providers.Provider{
		"anthropic": {Name: "anthropic", Provider: "anthropic", Auth: providers.Auth{Type: "key"}},
		"gh":        {Name: "gh", Provider: "github", Auth: providers.Auth{Type: providers.AuthCLI}},
		"sub":       {Name: "sub", Provider: "anthropic", Auth: providers.Auth{Type: providers.AuthCLI}},
	}}
	if rows := subscriptionRows(reg, "claude"); len(rows) != 1 || rows[0].Name != "sub" {
		t.Errorf("subscriptionRows = %v, want only the cli-auth anthropic row", rows)
	}
}
