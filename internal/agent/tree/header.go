package tree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent/serve"
	"github.com/edotau/claude-code/internal/dev/review/bench"
	"github.com/edotau/claude-code/internal/platform/textutil"
)

const (
	headerEvery        = 5 * time.Second
	headerServeTimeout = 300 * time.Millisecond
)

type headerStatus struct {
	bench      bench.Status
	serve      serve.Status
	served     bool
	serveError bool
}

// headerSource is replaceable so dashboard tests never launch the worktree status verb.
var headerSource = loadHeaderStatus

func loadHeaderStatus(ctx context.Context, top string) headerStatus {
	s, err := serve.QueryStatus(headerServeTimeout)
	h := headerStatus{serve: s, served: err == nil}
	h.serveError = err != nil && !errors.Is(err, serve.ErrNotRunning) && !errors.Is(err, serve.ErrUnsupported) &&
		!errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) // a stale socket is a stopped daemon
	if ctx.Err() == nil {
		h.bench = bench.BuildStatus(ctx, top, time.Now())
	}
	return h
}

// refreshHeader loads off the UI loop, since BuildStatus spawns `worktree list`; it ends with ctx.
func refreshHeader(ctx context.Context, top string, updates chan<- headerStatus) {
	ticker := time.NewTicker(headerEvery)
	defer ticker.Stop()
	for ctx.Err() == nil {
		next := headerSource(ctx, top)
		if ctx.Err() != nil {
			return
		}
		select {
		case updates <- next:
		case <-ctx.Done():
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

func headerLines(h headerStatus, width int) []string {
	width = min(max(width, 1), maxLineWidth)
	return []string{
		textutil.TruncateCells(serveLine(h), width, "…"),
		textutil.TruncateCells(benchLine(h.bench, width), width, "…"),
	}
}

func serveLine(h headerStatus) string {
	if h.serveError {
		return "serve: unreadable"
	}
	if !h.served {
		return "serve: off"
	}
	if len(h.serve.Pools) == 0 {
		return "serve: idle"
	}
	parts := make([]string, 0, len(h.serve.Pools))
	for _, p := range h.serve.Pools {
		parts = append(parts, fmt.Sprintf("%s %d/%d wait %d", p.Pool, p.Running, p.Cap, len(p.Waiting)))
	}
	return "serve: " + strings.Join(parts, " · ")
}

func benchLine(s bench.Status, width int) string {
	lanes, shortLanes := "lanes: unreadable", "lanes: unreadable"
	if s.LanesError == "" {
		benchCount, fixCount := 0, 0
		for _, lane := range s.Lanes {
			if strings.HasPrefix(lane.Name, "bench-") {
				benchCount++
			} else if strings.HasPrefix(lane.Name, "autofix-") {
				fixCount++
			}
		}
		lanes = fmt.Sprintf("lanes: %d/%d (bench %d, fix %d)", len(s.Lanes), s.LaneCap, benchCount, fixCount)
		shortLanes = fmt.Sprintf("lanes %d/%d", len(s.Lanes), s.LaneCap)
	}
	queue, shortQueue := "queue: unreadable", "q: unreadable"
	if s.QueueError == "" && s.Queue != nil {
		queue = fmt.Sprintf("queue: %d (fix %d)", s.Queue.Total, s.Queue.Counts["fix"])
		shortQueue = fmt.Sprintf("q%d", s.Queue.Total)
	}
	budget, shortBudget := "budget: unreadable", "budget: unreadable"
	if s.BudgetError == "" && s.Budget != nil {
		budget = fmt.Sprintf("budget: %d/%d tok", s.Budget.Tokens, s.Budget.TokenLimit)
		shortBudget = fmt.Sprintf("budget %d/%d", s.Budget.Tokens, s.Budget.TokenLimit)
	}
	if long := lanes + " · " + queue + " · " + budget; textutil.DisplayWidth(long) <= width {
		return long
	}
	return shortLanes + " · " + shortQueue + " · " + shortBudget
}
