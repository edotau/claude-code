package ask

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// runAskAndReport executes one prepared foreground dispatch and reports its outcome.
func runAskAndReport(ctx context.Context, client *agent.Client, p *agent.Prepared, isJob bool, format string) int {
	opts := agent.RunRecordedOpts{Job: isJob}
	if isJob && format == helptext.Text {
		opts.AnswerTo = os.Stdout // a --job child's stdout is the log: the full answer lands once at the end
	}
	res, err := client.Exec(ctx, p, stdoutSink(format, p.Record.ID), opts)
	return reportAsk(format, p, res, err)
}

// reportAsk prints one finished dispatch: its answer or envelope, then the stderr trailer; the exit code.
func reportAsk(format string, p *agent.Prepared, res wire.Result, err error) int {
	writeErr := reportAskResult(format, res, err)
	refused := p.Chain.Preflight != nil && len(p.Chain.Preflight.Failed) > 0
	for _, line := range askTrailer(format, res, refused) {
		if line != "" {
			fmt.Fprintln(os.Stderr, "ask:", line)
		}
	}
	if err := cmp.Or(err, writeErr); err != nil {
		return helptext.Fail("ask: %v", err)
	}
	return 0
}

// askTrailer is the stderr lines after a dispatch; the json envelope already carries ran_on and dispatch_id.
func askTrailer(format string, res wire.Result, refused bool) []string {
	lines := []string{ranOn(res), "", "dispatch " + res.DispatchID}
	if format == helptext.JSON {
		lines[0], lines[2] = "", ""
	}
	if !refused { // a refusal's preflight: line named the reason
		lines[1] = wire.DidNotRun(res)
	}
	return lines
}

// reportAskResult prints the outcome; it returns the stdout write error of the json envelope, else nil.
func reportAskResult(format string, res wire.Result, err error) error {
	for _, d := range res.Dropped {
		if d == "--read-only (tools off)" {
			fmt.Fprintf(os.Stderr, "ask: the %s agent runs with tools off (read-only sandbox where it has one)\n", res.Agent)
			continue
		}
		fmt.Fprintf(os.Stderr, "ask: %s has no channel on the %s agent — dropped\n", d, res.Agent)
	}
	if format == helptext.JSON {
		env := wire.NewEnvelope(res, err, res.DispatchID)
		env.RanOn = ranOn(res)
		return agent.WriteJSONLine(os.Stdout, env)
	}
	if format == agent.FormatStreamJSON {
		return nil // the result event already carried the envelope
	}
	if res.Answer != "" && !strings.HasSuffix(res.Answer, "\n") {
		fmt.Println()
	}
	return nil
}
