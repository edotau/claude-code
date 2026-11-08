package ask

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/agent/client"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// prepareExitCode is the code a Prepare error names, else 1.
func prepareExitCode(err error) int {
	if usage, ok := errors.AsType[interface {
		error
		ExitCode() int
	}](err); ok {
		return usage.ExitCode()
	}
	return 1
}

// askSpawn detaches a prepared dispatch as a background job.
func askSpawn(fs *flag.FlagSet, prepared *client.Prepared, native bool, format, name string) int {
	if err := job.Spawn(&prepared.Record, backgroundExtraFlags(fs, prepared.Opts.Fallback, native)); err != nil {
		return failAsk(format, name, err)
	}
	ReportBackgroundStart(format, prepared.Record)
	return 0
}

// absDebugFile pins the path now: --workdir moves the child's cwd.
func absDebugFile(p string) string {
	if p == "" {
		return p
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// runAskAndReport executes one prepared foreground dispatch and reports its outcome.
func runAskAndReport(ctx context.Context, client *client.Client, p *client.Prepared, isJob bool, format string) int {
	opts := job.RunRecordedOpts{Job: isJob}
	if isJob && format == helptext.Text {
		opts.AnswerTo = os.Stdout // a --job child's stdout is the log: the full answer lands once at the end
	}
	res, err := client.Exec(ctx, p, StdoutSink(format, p.Record.ID), opts)
	reportAskResult(format, res, err, res.DispatchID)
	for _, line := range []string{wire.RanOn(res), wire.DidNotRun(res)} {
		if line != "" {
			fmt.Fprintln(os.Stderr, "ask:", line)
		}
	}
	fmt.Fprintln(os.Stderr, "ask: dispatch", res.DispatchID, "(agents result <id> reprints it; agents ask --retry <id> replays it)")
	if err != nil {
		return helptext.Fail("ask: %v", err)
	}
	return 0
}

// The ask --format values.
const formatStreamJSON = client.FormatStreamJSON

// StdoutSink is the sink behind stdout for format: streamed text, nothing (json prints one envelope), or NDJSON.
func StdoutSink(format, dispatchID string) wire.Sink {
	switch format {
	case helptext.JSON:
		return wire.Discard
	case formatStreamJSON:
		return wire.NewJSONLSink(os.Stdout, dispatchID)
	}
	return wire.NewTextSink(os.Stdout)
}
