package job

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/shapes"
)

// serveLive is the ServeLive seam of RunHolder.
var serveLive = ServeLive

// RunHolder is the --job child of `ask --live`: it serves rec's session until idle, cancel or the agent's exit.
func RunHolder(rec *Record, launcher agent.Launcher) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	rec.Request.Launcher = launcher // json:"-", so the record loaded from disk has none
	if err := serveLive(ctx, rec); err != nil {
		rec.Status, rec.Error = "error", shapes.Redact(err.Error())
		rec.DurationMs = time.Since(rec.Time).Milliseconds()
		_ = rec.Save()
		fmt.Fprintf(os.Stderr, "claude-code: ask --live: %v\n", err)
		return 1
	}
	return 0
}
