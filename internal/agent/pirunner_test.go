package agent

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
)

// A databricks:<p> confinement plans no pi hop on a sibling workspace and mints no token for one.
func TestPiRunnerConfinedStaysOnWorkspace(t *testing.T) {
	reg, _, _ := autoFixture(t)
	q := RouteReq{Agent: "claude", Model: "databricks-claude-sonnet-5-5", Fallback: router.FallbackAll}
	lead := Dispatch{Account: "databricks·dev", Model: q.Model}
	open := proc.WithValues(context.Background(), proc.Values{})
	sibs, skips, _ := piSiblings(open, reg, q, lead, q.Model)
	if len(sibs)+len(skips) == 0 {
		t.Fatal("fixture: an unconfined ask considers no sibling workspace")
	}
	armed := proc.WithValues(context.Background(), proc.Values{route.OverlayVar: `{"provider":"databricks","profile":"dev"}`})
	if sibs, skips, why := piSiblings(armed, reg, q, lead, q.Model); len(sibs)+len(skips) != 0 || why != "" {
		t.Fatalf("confined to dev, pi hops planned beyond it: hops %v, skips %v, why %q", sibs, skips, why)
	}
	testTokenScope(t, reg, open, armed)
}

// testTokenScope: confined to dev scopes dev only; unconfined keeps asked + extra + fallbacks; a bad overlay refuses.
func testTokenScope(t *testing.T, reg *providers.Registry, open, armed context.Context) {
	t.Helper()
	if _, err := TokenScope(armed, "qa"); err == nil {
		t.Fatal("confined to dev, a token for qa was scoped")
	}
	if got, err := TokenScope(armed, "dev"); err != nil || len(got) != 1 || got[0] != "dev" {
		t.Fatalf("confined token scope = %v, %v; want [dev]", got, err)
	}
	want := slices.Compact(slices.Sorted(slices.Values(append([]string{"dev", "qa"}, reg.Fallback...))))
	if got, err := TokenScope(open, "dev", "qa"); err != nil || !slices.Equal(got, want) {
		t.Fatalf("unconfined token scope = %v, %v; want asked + extra + fallbacks %v", got, err, want)
	}
	bad := proc.WithValues(context.Background(), proc.Values{route.OverlayVar: `{"provider":`})
	if got, err := TokenScope(bad, "dev"); err == nil {
		t.Fatalf("malformed overlay scoped %v; want a refusal", got)
	}
}

// ledBy with no hops leaves the chain as it was instead of indexing an empty list.
func TestLedByNoHops(t *testing.T) {
	c := Chain{Want: "claude", Dispatches: []Dispatch{{Model: "m"}}, Preflight: &Preflight{Chosen: "x"}}
	got := c.ledBy(nil)
	if len(got.Dispatches) != 1 || got.Preflight.Chosen != "x" {
		t.Fatalf("ledBy(nil) = %+v", got)
	}
}

// The inner line of a --carry runs the carried agent's own CLI; Pi keeps the takeover.
func TestPiRunnerBarredForCarriedChild(t *testing.T) {
	setConfigDir(t)
	now := time.Now()
	record := func(i int, carry bool, status string, pid int) Record {
		r := NewRecord("codex", RunOptions{Carry: carry, Prompt: "the task"}, now.Add(time.Duration(i)*time.Second))
		r.Status, r.PID = status, pid
		r.PIDStart, _ = proc.StartTime(pid)
		if err := r.Save(); err != nil {
			t.Fatal(err)
		}
		return r
	}
	rec := record(0, true, "running", os.Getppid()) // the carrier is this test's parent process
	ctx := proc.WithValues(context.Background(), proc.Values{proc.DispatchIDEnv: rec.ID})
	if why := piRunnerBarred(ctx, RouteReq{Agent: "codex"}); !strings.Contains(why, "carried") {
		t.Fatalf("carried codex child: barred = %q, want the carried reason", why)
	}
	if why := piRunnerBarred(ctx, RouteReq{Agent: "gemini"}); why != "" {
		t.Fatalf("another agent under a codex carrier is barred: %q", why)
	}
	done, plain := record(1, true, "ok", os.Getppid()), record(2, false, "running", os.Getppid())
	noPID := record(3, true, "running", 0)
	for _, id := range []string{done.ID, plain.ID, noPID.ID, "../" + rec.ID} {
		ctx := proc.WithValues(context.Background(), proc.Values{proc.DispatchIDEnv: id})
		if why := piRunnerBarred(ctx, RouteReq{Agent: "codex"}); why != "" {
			t.Fatalf("parent %q is no live carrier, yet barred: %q", id, why)
		}
	}
}
