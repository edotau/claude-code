package batch

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
)

// Opts are the ask flags that apply to a whole --batch run; Identity is the --as value, empty when unset.
type Opts struct {
	Jobs            int
	FailFast        bool
	Agent, Fallback string
	Native          bool
	Identity        string
}

// BuildTask validates one line like the ask flags and attaches the routing step; routing waits for Prepare.
func BuildTask(l ParsedLine, o Opts, base agent.RunOptions, timeoutSet bool, cwd string, seen map[string]bool,
	newClient func() *agent.Client) (BatchTask, error) {
	t, err := ParseBatchTask(l, o.Agent, base, timeoutSet, cwd, seen)
	if err != nil {
		return t, err
	}
	req := t.Req
	req.Agent, req.Fallback, req.Native = t.Agent, o.Fallback, o.Native
	client := newClient()
	t.Prepare = func(ctx context.Context) (*agent.Prepared, error) {
		p, err := client.Prepare(ctx, req, nil)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", l.N, err)
		}
		return p, nil
	}
	return t, nil
}

// Run builds a task per line (a bad line fails alone, logged to os.Stderr) and runs them on the pool; true only if all ok.
func Run(ctx context.Context, lines []ParsedLine, o Opts, base agent.RunOptions, timeoutSet bool,
	newClient func() *agent.Client, out io.Writer) (id string, ok bool) {
	if o.Identity != "" {
		ctx = providers.WithIdentity(ctx, o.Identity)
	}
	cwd, _ := os.Getwd() // resolved once here: a pool goroutine must never fall back to the process cwd
	seen := map[string]bool{}
	tasks := make([]BatchTask, 0, len(lines))
	for _, l := range lines {
		t, err := BuildTask(l, o, base, timeoutSet, cwd, seen, newClient)
		if err != nil {
			t.Err = err
			fmt.Fprintf(os.Stderr, "claude-code: ask: %v\n", err)
		}
		tasks = append(tasks, t)
	}
	id = NewBatchID()
	return id, RunBatch(ctx, tasks, BatchOptions{ID: id, Jobs: o.Jobs, FailFast: o.FailFast, Out: out, Log: os.Stderr})
}
