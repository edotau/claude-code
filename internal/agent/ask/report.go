package ask

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// runAskAndReport executes one prepared foreground dispatch and reports its outcome.
func runAskAndReport(ctx context.Context, client *agent.Client, p *agent.Prepared, isJob bool, format, save string,
	out io.Writer) int {
	res, err := client.Exec(ctx, p, sinkTo(sinkOut(out, format, save), format, p.Record.ID), jobOpts(isJob, format, out))
	return reportAsk(out, format, save, p, res, err)
}

// saveHead is how many answer lines stdout keeps when --save files the whole answer.
const saveHead = 8

// sinkOut is where the text stream goes: nowhere under --save, which prints the head after the run.
func sinkOut(out io.Writer, format, save string) io.Writer {
	if save != "" && format == helptext.Text {
		return io.Discard
	}
	return out
}

// saveAnswer files a finished run's answer at path (a read-only leaf's report); "" saves nothing.
func saveAnswer(path string, res wire.Result) error {
	if path == "" || res.Answer == "" {
		return nil
	}
	if err := paths.AtomicWrite(path, []byte(res.Answer), 0o600); err != nil {
		return fmt.Errorf("save %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "ask: saved %d B -> %s\n", len(res.Answer), path)
	return nil
}

// printHead is the text answer's first saveHead lines, which the silenced sink left unprinted.
func printHead(out io.Writer, res wire.Result) error {
	head := firstLines(res.Answer, saveHead)
	if head != "" && !strings.HasSuffix(head, "\n") {
		head += "\n"
	}
	_, err := io.WriteString(out, head)
	return err
}

// jobOpts is the recorded-run options of a foreground ask; a --job child's stdout is the log, so the answer lands once.
func jobOpts(isJob bool, format string, out io.Writer) agent.RunRecordedOpts {
	opts := agent.RunRecordedOpts{Job: isJob}
	if isJob && format == helptext.Text {
		opts.AnswerTo = out
	}
	return opts
}

// reportAsk prints one finished dispatch: its answer or envelope, then the stderr trailer; the exit code.
func reportAsk(out io.Writer, format, save string, p *agent.Prepared, res wire.Result, err error) int {
	writeErr := reportAskResult(sinkOut(out, format, save), format, res, err)
	if save != "" {
		writeErr = cmp.Or(writeErr, reportSaved(out, format, save, res, err))
	}
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

// reportSaved saves a successful run's answer and, in text format, prints its head; a failed run saves nothing.
func reportSaved(out io.Writer, format, save string, res wire.Result, runErr error) error {
	var saveErr error
	if runErr == nil {
		saveErr = saveAnswer(save, res)
	}
	if format != helptext.Text {
		return saveErr
	}
	return cmp.Or(saveErr, printHead(out, res))
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
func reportAskResult(out io.Writer, format string, res wire.Result, err error) error {
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
		return agent.WriteJSONLine(out, env)
	}
	if format == agent.FormatStreamJSON {
		return nil // the result event already carried the envelope
	}
	if res.Answer != "" && !strings.HasSuffix(res.Answer, "\n") {
		_, werr := fmt.Fprintln(out)
		return werr
	}
	return nil
}
