package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/batch"
	"github.com/edotau/claude-code/internal/providers"
)

// batchOpts are the ask flags that apply to a whole --batch run.
type batchOpts struct {
	jobs            int
	failFast        bool
	agent, fallback string
	native          bool
}

// batchExclusive are the ask flags that make no sense on a batch of independent tasks.
var batchExclusive = []string{"retry", "job", "background", "session", "log", "attach", "rubric", "debug-file"}

// runAskBatch runs the JSONL tasks in src (a file, or "-" for stdin) on a bounded pool and streams one envelope per line.
func runAskBatch(fs *flag.FlagSet, resume bool, src string, o batchOpts, base agent.RunOptions) int {
	var bad error
	if resume || fs.NArg() > 0 {
		bad = errors.New("--batch takes tasks from the JSONL source, not a session id or a positional task")
	}
	for _, f := range batchExclusive {
		if flagExplicit(fs, f) {
			bad = fmt.Errorf("--batch cannot be combined with --%s", f)
		}
	}
	if flagExplicit(fs, "format") && fs.Lookup("format").Value.String() != formatJSON {
		bad = errors.New("--batch always prints JSON envelopes; drop --format or pass --format json")
	}
	if bad == nil && o.jobs < 1 {
		bad = fmt.Errorf("--jobs %d: need at least 1", o.jobs)
	}
	if bad != nil {
		return failAskCode("text", o.agent, bad, 2)
	}
	in := io.Reader(os.Stdin)
	if src != "-" {
		f, err := os.Open(src)
		if err != nil {
			return failAskCode("text", o.agent, fmt.Errorf("--batch: %w", err), 2)
		}
		defer f.Close()
		in = f
	}
	lines, err := batch.ReadTaskLines(in)
	if err != nil {
		return failAskCode("text", o.agent, fmt.Errorf("--batch: %w", err), 2)
	}
	if len(lines) == 0 {
		return failAskCode("text", o.agent, errors.New("--batch: no tasks in the input"), 2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGPIPE) // a closed stdout cancels
	defer stop()
	if as := fs.Lookup("as"); as != nil {
		ctx = providers.WithIdentity(ctx, as.Value.String())
	}
	cwd, _ := os.Getwd() // resolved once here: a pool goroutine must never fall back to the process cwd
	timeoutSet := flagExplicit(fs, "timeout")
	seen := map[string]bool{}
	tasks := make([]batch.BatchTask, 0, len(lines))
	for _, l := range lines {
		t, err := batchTask(l, o, base, timeoutSet, cwd, seen)
		if err != nil {
			t.Err = err
			fail("ask: %v", err)
		}
		tasks = append(tasks, t)
	}
	id := batch.NewBatchID()
	ok := batch.RunBatch(ctx, tasks, batch.BatchOptions{ID: id, Jobs: o.jobs, FailFast: o.failFast, Out: os.Stdout, Log: os.Stderr})
	if !ok {
		fmt.Fprintf(os.Stderr, "ask: %s: not every task was ok\n", id)
		return 1
	}
	return 0
}

// batchTask validates one line like the ask flags and attaches the routing step; routing waits for Prepare.
func batchTask(l batch.ParsedLine, o batchOpts, base agent.RunOptions, timeoutSet bool, cwd string,
	seen map[string]bool) (batch.BatchTask, error) {
	t, err := batch.ParseBatchTask(l, o.agent, base, timeoutSet, cwd, seen)
	if err != nil {
		return t, err
	}
	req := t.Req
	req.Agent, req.Fallback, req.Native = t.Agent, o.fallback, o.native
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
