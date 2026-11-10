package client

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/edotau/claude-code/internal/agent"

	"github.com/edotau/claude-code/internal/agent/pack"
	"github.com/edotau/claude-code/internal/models"
)

// packReserveTokens is the output headroom in the prompt-cap rule: window − 32k − 10%.
const packReserveTokens = 32_000

// packFile is a pack the agent reads from the workdir instead of the prompt (CLI.PackVia == PackFile).
type packFile struct{ path, body string }

func (f *packFile) write() error {
	out, err := os.OpenFile(f.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("pack file: %w", err)
	}
	_, err = out.WriteString(f.body)
	if err = errors.Join(err, out.Close()); err != nil {
		return fmt.Errorf("pack file: %w", errors.Join(err, os.Remove(f.path)))
	}
	return nil
}

func (f *packFile) remove() { _ = os.Remove(f.path) }

// packBudget is the byte budget for d: its model window under the prompt-cap rule, then its transport cap
// unless the pack travels as a file (the prompt then carries only the instruction).
func packBudget(d agent.Dispatch, req agent.RunOptions, viaFile bool) (budget int, note string) {
	s := agent.SchemaFor(d.Agent.Name())
	model := d.Model
	if model == "" {
		if t, _, err := s.Target(req); err == nil {
			model = t.Model
		}
	}
	if model == "" {
		note = d.Agent.Name() + ": model unknown; packing to the 200K-token default window"
	}
	budget = pack.BudgetBytes(models.PromptTokens(models.Window(model), packReserveTokens))
	if n := cmp.Or(s.CLI.PromptMaxBytes, s.CLI.PromptVia.MaxBytes()); n > 0 && n < budget && !viaFile {
		budget, note = n, d.Agent.Name()+": "+s.CLI.PromptVia.String()+" prompt cap"
	}
	return budget, note
}

// packPrompt packs p.Opts.Pack around the task for the lead dispatch, drops later dispatches the pack would
// not fit, and either inlines the pack in the prompt or, for a PackFile lead, stages it as a file to read.
func (c *Client) packPrompt(ctx context.Context, p *Prepared) error {
	o := p.Opts
	if len(p.Chain.Dispatches) == 0 {
		return nil // routing already recorded why nothing can run
	}
	cwd, err := filepath.Abs(o.WorkDir) // "" is the process cwd; the child starts inside it, so the prompt path must be absolute
	if err != nil {
		return clientUsageError{err}
	}
	lead := p.Chain.Dispatches[0]
	viaFile := agent.SchemaFor(lead.Agent.Name()).CLI.PackVia == agent.PackFile
	budget, note := packBudget(lead, o, viaFile)
	packed, err := pack.Build(ctx, *o.Pack, o.Prompt, cwd, budget)
	if err != nil {
		return clientUsageError{err}
	}
	notes := append(packed.Notes, note)
	if viaFile {
		p.pack = &packFile{path: filepath.Join(cwd, pack.FilePrefix+p.Record.ID+".md"), body: packed.Prompt}
		p.Opts.Prompt = o.Prompt + "\n\nThe workspace context for this task is in " + p.pack.path +
			". Read that file in full with your file tools before answering."
		p.Opts.Tools, p.Record.Request.Tools = true, true
		notes = append(notes, fmt.Sprintf("%s: pack (%d KB) is read from a file; tools are on for this run (read-only intent)",
			lead.Agent.Name(), len(packed.Prompt)/1024))
	} else {
		p.Opts.Prompt = packed.Prompt
	}
	for _, n := range notes {
		if n != "" && c.Notify != nil {
			c.Notify(n)
		}
	}
	p.Chain = fitChain(p.Chain, len(packed.Prompt), o)
	return nil
}

// fitChain keeps the lead and every later dispatch whose budget holds n bytes; the rest become skip reasons.
func fitChain(c agent.Chain, n int, o agent.RunOptions) agent.Chain {
	out := agent.Chain{Want: c.Want, Dispatches: c.Dispatches[:1:1], Skipped: slices.Clone(c.Skipped)}
	for _, d := range c.Dispatches[1:] {
		if b, _ := packBudget(d, o, false); b < n {
			out.Skipped = append(out.Skipped, fmt.Sprintf("%s: packed prompt (%d KB) is over its %d KB budget", d.Agent.Name(), n/1024, b/1024))
			continue
		}
		out.Dispatches = append(out.Dispatches, d)
	}
	return out
}
