package hooks

import (
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hookspec"
)

func bindStub(group, name string, budget time.Duration, run Runner) Bound {
	return Bound{Group: group, Spec: hookspec.EventSpec{Name: name, Event: "UserPromptSubmit", Budget: budget}, Run: run}
}

func TestDispatchCapsMergedContextLaterBlockFirst(t *testing.T) {
	a := strings.Repeat("a", 6000)
	b := strings.Repeat("b", 6000)
	bound := []Bound{
		bindStub("g1", "h1", time.Second, func(*Input) Result { return Result{Context: a} }),
		bindStub("g2", "h2", time.Second, func(*Input) Result { return Result{Context: b} }),
	}
	res, _ := Dispatch(bound, "UserPromptSubmit", &Input{}, io.Discard)
	if n := len([]rune(res.Context)); n > 10000 || !strings.HasPrefix(res.Context, a) || !strings.HasSuffix(res.Context, "…truncated") {
		t.Fatalf("len=%d; want <=10000, first block intact, truncated marker", n)
	}
}

func TestDispatchWaitsForOverrunHandler(t *testing.T) {
	var finished atomic.Bool
	bound := []Bound{bindStub("g1", "slow", 40*time.Millisecond, func(*Input) Result {
		time.Sleep(70 * time.Millisecond)
		finished.Store(true)
		return Result{}
	})}
	Dispatch(bound, "UserPromptSubmit", &Input{}, io.Discard)
	if !finished.Load() {
		t.Fatal("Dispatch returned while an overrun handler was still running")
	}
}
