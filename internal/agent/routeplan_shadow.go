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

// parentRoute is what a dispatch inherits: an open fence, or a --retry plan's, and whether it is a carried child.
func parentRoute(ctx context.Context) ParentRoute {
	pr := ParentRoute{Fence: route.Fence{Open: true}, Carried: carriedChild(ctx, "")}
	if r, ok := RetriedOf(ctx); ok && r.Plan != nil {
		pr.Replay = r.Plan
		if f := r.Plan.Fence; f.Open || len(f.Profiles) > 0 { // a zero fence is a plan that lacked one
			pr.Fence = f
		}
	}
	return pr
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
