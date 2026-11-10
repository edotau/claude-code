package job

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/textutil"
)

// serveLive is the ServeLive seam of RunHolder.
var serveLive = ServeLive

// RunHolder is the --job child of `ask --live`: it serves rec's session until idle, cancel or the agent's exit.
func RunHolder(rec *Record, launcher agent.Launcher) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	rec.Request.Launcher = launcher // json:"-", so the record loaded from disk has none
	if err := serveLive(ctx, rec); err != nil {
		msg := textutil.RedactSecrets(err.Error()) // stderr is the job log: the same redaction as the record
		rec.Status, rec.Error = "error", msg
		rec.DurationMs = time.Since(rec.Time).Milliseconds()
		_ = rec.Save()
		fmt.Fprintf(os.Stderr, "claude-code: ask --live: %s\n", msg)
		return 1
	}
	return 0
}
