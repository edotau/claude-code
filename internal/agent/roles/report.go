package roles

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/agent"
)

// ReportSchema versions the Report JSON.
const ReportSchema = 1

// streamLeg is the per-leg dispatch seam for Run.
var streamLeg = (*agent.Client).Stream

// Report is a role run: Status is ok (all legs ok), partial (at least one) or error (none).
type Report struct {
	Schema int         `json:"schema"`
	Role   string      `json:"role"`
	Agent  string      `json:"agent"`
	Status string      `json:"status"`
	Legs   []LegResult `json:"legs"`
}

// LegResult is one leg's label and its `ask --format json` envelope.
type LegResult struct {
	Label    string         `json:"label"`
	Envelope agent.Envelope `json:"envelope"`
}

// Run dispatches legs concurrently, host-isolated under a limit of min(jobs, 12), tagging progress lines with the label.
func Run(ctx context.Context, c *agent.Client, r Role, legs []Leg, jobs int, progress io.Writer) Report {
	ctx = agent.WithBatchRun(ctx, agent.NewBatchLimiter(min(jobs, MaxParts)))
	rep := Report{Schema: ReportSchema, Role: r.Name, Agent: r.Agent, Legs: make([]LegResult, len(legs))}
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i, leg := range legs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rep.Legs[i] = runLeg(ctx, c, leg, progress, &mu)
		}()
	}
	wg.Wait()
	rep.Status = status(rep.Legs)
	return rep
}

func runLeg(ctx context.Context, c *agent.Client, leg Leg, progress io.Writer, mu *sync.Mutex) LegResult {
	if leg.Err != "" {
		return LegResult{Label: leg.Label, Envelope: failed(leg, leg.Err)}
	}
	env := failed(leg, "no result event")
	var line []byte // text events are chunks: only whole lines are tagged and written, so legs never interleave mid-line
	flush := func(b []byte) {
		mu.Lock()
		_, _ = fmt.Fprintf(progress, "[%s] %s\n", leg.Label, b)
		mu.Unlock()
	}
	for ev := range streamLeg(c, ctx, leg.Opts) {
		switch {
		case ev.Type == agent.EventText && progress != nil:
			if ev.Sep && len(line) > 0 {
				line = append(line, '\n')
			}
			line = append(line, ev.Text...)
			for i := bytes.IndexByte(line, '\n'); i >= 0; i = bytes.IndexByte(line, '\n') {
				flush(line[:i])
				line = line[i+1:]
			}
		case ev.Type == agent.EventResult && ev.Envelope != nil:
			env = *ev.Envelope
		}
	}
	if len(line) > 0 {
		flush(line)
	}
	return LegResult{Label: leg.Label, Envelope: env}
}

func failed(leg Leg, msg string) agent.Envelope {
	env := agent.FailureEnvelope(leg.Opts.Agent, fmt.Errorf("%s", msg))
	if env.Model == "" {
		env.Model = leg.Opts.Model
	}
	return env
}

func status(legs []LegResult) string {
	ok := 0
	for _, l := range legs {
		if l.Envelope.Status == "ok" {
			ok++
		}
	}
	switch ok {
	case len(legs):
		return "ok"
	case 0:
		return "error"
	}
	return "partial"
}

// Text prints each leg as `### <label> (<agent>/<model>) [<status>]` and its answer or error, then the failed labels.
func (rep Report) Text(w io.Writer) {
	var bad []string
	for _, l := range rep.Legs {
		e := l.Envelope
		_, _ = fmt.Fprintf(w, "### %s (%s/%s) [%s]\n", l.Label, e.Agent, e.Model, e.Status)
		if e.Status == "ok" {
			_, _ = fmt.Fprintln(w, e.Answer)
			continue
		}
		bad = append(bad, l.Label)
		if e.Error != nil {
			_, _ = fmt.Fprintln(w, e.Error.Message)
		}
	}
	if len(bad) > 0 {
		_, _ = fmt.Fprintf(w, "failed: %s\n", strings.Join(bad, ", "))
	}
}
