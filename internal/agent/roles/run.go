package roles

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent/client"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
)

// ReportSchema versions the Report JSON.
const ReportSchema = 1

// streamLeg is the per-leg dispatch seam for Run.
var streamLeg = (*client.Client).Stream

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
	Label    string        `json:"label"`
	Envelope wire.Envelope `json:"envelope"`
}

// Run dispatches legs concurrently, host-isolated under a limit of min(jobs, 12), tagging progress lines with the label.
func Run(ctx context.Context, c *client.Client, r Role, legs []Leg, jobs int, progress io.Writer) Report {
	ctx = agent.WithBatchRun(ctx, retry.NewBatchLimiter(min(jobs, MaxParts)))
	rep := Report{Schema: ReportSchema, Role: r.Name, Agent: r.Agent, Legs: make([]LegResult, len(legs))}
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i, leg := range legs {
		wg.Go(func() {
			rep.Legs[i] = runLeg(ctx, c, leg, progress, &mu)
		})
	}
	wg.Wait()
	rep.Status = status(rep.Legs)
	return rep
}

func runLeg(ctx context.Context, c *client.Client, leg Leg, progress io.Writer, mu *sync.Mutex) LegResult {
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
		case ev.Type == wire.EventText && progress != nil:
			line = progressLines(line, ev, flush)
		case ev.Type == wire.EventResult && ev.Envelope != nil:
			env = *ev.Envelope
		}
	}
	if len(line) > 0 {
		flush(line)
	}
	return LegResult{Label: leg.Label, Envelope: env}
}

func progressLines(line []byte, ev wire.Event, flush func([]byte)) []byte {
	if ev.Sep && len(line) > 0 {
		line = append(line, '\n')
	}
	line = append(line, ev.Text...)
	for i := bytes.IndexByte(line, '\n'); i >= 0; i = bytes.IndexByte(line, '\n') {
		flush(line[:i])
		line = line[i+1:]
	}
	return line
}

func failed(leg Leg, msg string) wire.Envelope {
	env := wire.FailureEnvelope(leg.Opts.Agent, fmt.Errorf("%s", msg))
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
// It returns the first write error; bufio keeps the error sticky, so Flush reports any failed write.
func (rep Report) Text(w io.Writer) error {
	bw := bufio.NewWriter(w)
	var bad []string
	for _, l := range rep.Legs {
		e := l.Envelope
		_, _ = fmt.Fprintf(bw, "### %s (%s/%s) [%s]\n", l.Label, e.Agent, e.Model, e.Status)
		if e.Status == "ok" {
			_, _ = fmt.Fprintln(bw, e.Answer)
			continue
		}
		bad = append(bad, l.Label)
		if e.Error != nil {
			_, _ = fmt.Fprintln(bw, e.Error.Message)
		}
	}
	if len(bad) > 0 {
		_, _ = fmt.Fprintf(bw, "failed: %s\n", strings.Join(bad, ", "))
	}
	return bw.Flush()
}

// Request is the verb's input; Parts are explore sub-questions, Models override a compare role's default set.
type Request struct {
	Task, WorkDir string
	Parts, Models []string
	Web           bool
	Timeout       time.Duration
	OutputSchema  string
}

// Leg is one dispatch of a role; a non-empty Err means it failed before dispatch.
type Leg struct {
	Label string
	Opts  agent.RunOptions
	Err   string
}

// Legs builds r's legs for q; the shape follows the role name, the agent comes from the table.
func Legs(r Role, q Request) []Leg {
	base := agent.RunOptions{Agent: r.Agent, WorkDir: q.WorkDir, Timeout: q.Timeout, OutputSchema: q.OutputSchema,
		Permissions: r.Permissions}
	switch r.Name {
	case "explore":
		return exploreLegs(base, q)
	case "fleet":
		o := base
		o.Fleet, o.Tools, o.Prompt = true, true, q.Task
		return []Leg{{Label: "fleet", Opts: o}}
	case "compare":
		return compareLegs(r, base, q)
	}
	return nil
}

func exploreLegs(base agent.RunOptions, q Request) []Leg {
	parts := q.Parts
	if len(parts) == 0 {
		parts = []string{q.Task}
	}
	legs := make([]Leg, 0, len(parts))
	for _, p := range parts {
		o, err := Options(base, p, q.Web)
		leg := Leg{Label: p, Opts: o}
		if err != nil { // keep the requested agent for the failure envelope; Options returns zeroed opts on refusal
			leg.Opts, leg.Err = base, err.Error()
		}
		legs = append(legs, leg)
	}
	return legs
}

func compareLegs(r Role, base agent.RunOptions, q Request) []Leg {
	models := q.Models
	if len(models) == 0 {
		models = r.Models
	}
	menu := map[string]bool{}
	for _, m := range providers.OpencodeMenu() {
		menu[m.ID] = true
	}
	legs := make([]Leg, 0, len(models))
	for _, m := range models {
		o := base
		o.Model, o.Prompt = m, q.Task
		leg := Leg{Label: m, Opts: o}
		if r.Agent == "opencode" && !menu[m] {
			leg.Err = "not on the opencode menu"
		}
		legs = append(legs, leg)
	}
	return legs
}
