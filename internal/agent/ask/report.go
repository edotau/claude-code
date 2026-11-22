package ask

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/redact"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// runAskAndReport executes one prepared foreground dispatch and reports its outcome.
func runAskAndReport(ctx context.Context, client *agent.Client, p *agent.Prepared, isJob bool, format, save string,
	out io.Writer) int {
	res, err := client.Exec(ctx, p, sinkTo(sinkOut(out, format, save), format, p.Record.ID), jobOpts(isJob, format, out))
	return reportAsk(out, format, save, p, res, err)
}

func maskedOut(out io.Writer) io.WriteCloser { return redact.Writer(out) }

func warmMask() { redact.Shared() }

func closeAskOutput(out io.Closer, exit int) int {
	if err := out.Close(); err != nil {
		msg := strings.NewReplacer("\r", " ", "\n", " ").Replace(redact.Text(err.Error()))
		return cmp.Or(exit, helptext.Fail("ask: flush output: %s", msg))
	}
	return exit
}

type maskedEvents struct{ sink wire.Sink }

func (s maskedEvents) Emit(ev wire.Event) error {
	ev.Text = redact.Text(ev.Text)
	if ev.Envelope != nil {
		env := maskEnvelope(*ev.Envelope)
		ev.Envelope = &env
	}
	return s.sink.Emit(ev)
}

// writeEnvelope is the one way to print an envelope as a JSON line: masked, so no verb can forget it.
func writeEnvelope(w io.Writer, env wire.Envelope) error {
	return agent.WriteJSONLine(w, maskEnvelope(env))
}

func maskEnvelope(env wire.Envelope) wire.Envelope {
	env.Answer = redact.Text(env.Answer)
	env.FallbackReason = redact.Text(env.FallbackReason)
	env.RequestedWhy = redact.Text(env.RequestedWhy)
	env.Handoff = redact.Text(env.Handoff)
	if env.Error != nil {
		e := *env.Error
		e.Message = redact.Text(e.Message)
		env.Error = &e
	}
	env.Hops = slices.Clone(env.Hops)
	for i := range env.Hops {
		errs := slices.Clone(env.Hops[i].Errors)
		for j := range errs {
			errs[j].Message = redact.Text(errs[j].Message)
		}
		env.Hops[i].Errors = errs
	}
	if env.Denials != nil {
		d := *env.Denials
		d.Commands = slices.Clone(d.Commands)
		for i, command := range d.Commands {
			d.Commands[i] = redact.Text(command)
		}
		env.Denials = &d
	}
	env.Structured = maskStructured(env.Structured)
	return env
}

func maskStructured(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if dec.Decode(&value) != nil {
		return nil
	}
	changed := false
	masked, err := json.Marshal(maskJSONValue(value, &changed))
	if !changed {
		return raw // re-encoding would reorder keys for no reason
	}
	if err != nil {
		return nil
	}
	return masked
}

func maskJSONValue(value any, changed *bool) any {
	mask := func(s string) string {
		m := redact.Text(s)
		*changed = *changed || m != s
		return m
	}
	switch v := value.(type) {
	case string:
		return mask(v)
	case []any:
		for i, item := range v {
			v[i] = maskJSONValue(item, changed)
		}
	case map[string]any:
		masked := make(map[string]any, len(v))
		for key, item := range v {
			masked[mask(key)] = maskJSONValue(item, changed)
		}
		return masked
	}
	return value
}

// denialSummaryCells is the widest the sandbox summary line prints, in terminal columns.
const denialSummaryCells = 80

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
	answer := redact.Text(res.Answer)
	if err := paths.AtomicWrite(path, []byte(answer), 0o600); err != nil {
		return fmt.Errorf("save %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "ask: saved %d B -> %s\n", len(answer), path)
	return nil
}

// printHead is the text answer's first saveHead lines, which the silenced sink left unprinted.
func printHead(out io.Writer, res wire.Result) error {
	head := firstLines(redact.Text(res.Answer), saveHead)
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
	if rec, lerr := agent.LoadRecord(p.Record.ID); lerr == nil {
		if hint := agent.ResumeHint(rec); hint != "" {
			fmt.Fprintln(os.Stderr, hint)
		}
	}
	if err := cmp.Or(err, writeErr); err != nil {
		if errors.Is(err, agent.ErrBadAttach) || errors.Is(err, agent.ErrPromptTooLarge) { // refused before launch
			return helptext.FailUsage("ask: %v", err)
		}
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
	if headErr := printHead(out, res); headErr != nil {
		return cmp.Or(saveErr, headErr)
	}
	return cmp.Or(saveErr, printDenials(out, res))
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
		return writeEnvelope(out, env)
	}
	if format == agent.FormatStreamJSON {
		return nil // the result event already carried the envelope
	}
	if res.Answer != "" && !strings.HasSuffix(res.Answer, "\n") {
		_, werr := fmt.Fprintln(out)
		if werr != nil {
			return werr
		}
	}
	return printDenials(out, res)
}

func printDenials(out io.Writer, res wire.Result) error {
	d := res.Denials
	if d == nil || d.Count == 0 {
		return nil
	}
	noun := "commands"
	if d.Count == 1 {
		noun = "command"
	}
	head, tail := fmt.Sprintf("sandbox refused %d %s", d.Count, noun), "; re-run outside the sandbox"
	if len(d.Commands) > 0 {
		head += ": "
	}
	list := textutil.TruncateCells(strings.Join(d.Commands, ", "), max(0, denialSummaryCells-textutil.DisplayWidth(head+tail)), "…")
	_, err := fmt.Fprintln(out, head+list+tail)
	return err
}
