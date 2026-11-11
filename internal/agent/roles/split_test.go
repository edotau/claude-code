package roles

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent"
)

func TestSplitFallsBackOnError(t *testing.T) {
	orig := splitRun
	splitRun = func(*agent.Client, context.Context, agent.RunOptions, wire.Sink) (wire.Result, error) {
		return wire.Result{Answer: "a\nb"}, errors.New("down")
	}
	t.Cleanup(func() { splitRun = orig })
	if got := Split(context.Background(), nil, "q"); len(got) != 1 || got[0] != "q" {
		t.Errorf("got %v", got)
	}
}

func TestSplitCapsAtTwelve(t *testing.T) {
	orig := splitRun
	var lines []string
	for i := range 20 {
		lines = append(lines, fmt.Sprintf(" part %d ", i))
	}
	splitRun = func(*agent.Client, context.Context, agent.RunOptions, wire.Sink) (wire.Result, error) {
		return wire.Result{Answer: strings.Join(lines, "\n\n")}, nil
	}
	t.Cleanup(func() { splitRun = orig })
	got := Split(context.Background(), nil, "q")
	if len(got) != 12 || got[0] != "part 0" || got[11] != "part 11" {
		t.Errorf("len=%d first/last=%q/%q", len(got), got[0], got[len(got)-1])
	}
}

func TestSplitSingleLineReturnsQuestion(t *testing.T) {
	orig := splitRun
	splitRun = func(*agent.Client, context.Context, agent.RunOptions, wire.Sink) (wire.Result, error) {
		return wire.Result{Answer: "only one"}, nil
	}
	t.Cleanup(func() { splitRun = orig })
	if got := Split(context.Background(), nil, "q"); len(got) != 1 || got[0] != "q" {
		t.Errorf("got %v", got)
	}
}
