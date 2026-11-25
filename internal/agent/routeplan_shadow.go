package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/route"
)

// shadowPlan builds o's route plan beside ch, sets ch.Plan and logs where they differ; ch still runs unchanged.
func shadowPlan(ctx context.Context, o RunOptions, ch Chain) Chain {
	plan, err := PlanRoute(ctx, o, ParentRoute{Fence: route.Fence{Open: true}, Carried: carriedChild(ctx, "")})
	if err != nil {
		logDivergence(ctx, o.Agent, []string{err.Error()})
		return ch
	}
	ch.Plan = &plan
	logDivergence(ctx, o.Agent, Divergence(plan, ch))
	return ch
}

// logDivergence appends one JSON line to the shadow log when lines is non-empty; a write error never fails the run.
func logDivergence(ctx context.Context, agent string, lines []string) {
	if len(lines) == 0 || Peeking(ctx) {
		return
	}
	b, err := json.Marshal(map[string]any{"time": time.Now().UTC().Format(time.RFC3339), "agent": agent, "lines": lines})
	if err == nil {
		err = appendShadow(append(b, '\n'))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "notice: route plan shadow log: %v\n", err)
	}
}

func appendShadow(b []byte) error {
	path := filepath.Join(paths.StateDir(), "agent", "routeplan-shadow.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	return errors.Join(werr, f.Close())
}
