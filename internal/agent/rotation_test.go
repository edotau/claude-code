package agent

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

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
			m, _, err := rotationModelOn(rows, "opencode", models.Sonnet, true, false, time.Now())
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
