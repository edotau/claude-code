package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/router"
)

// An asked auto on a rotation row: a peek repeats and records nothing, a real pick stamps and advances, an empty pool ranks.
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
	rankedOnly(t)
	if r := resolve(context.Background()); r.Model == "" || r.Model == AutoModel || strings.Contains(r.Note, "rotation") {
		t.Errorf("empty pool ran %q (note %q), want the ranked pick", r.Model, r.Note)
	}
}
