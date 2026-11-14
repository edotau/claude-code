package waves

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
)

// Background detaches the wave runner using the shared dispatch records and spawn machinery.
func Background(ctx context.Context, o Options, args []string) int {
	rec := agent.ClaimRecord("waves", agent.RunOptions{WorkDir: o.Dir, Prompt: o.Plan, Background: true}, time.Now())
	rec.Repo, rec.Log = agent.RepoRoot(ctx, o.Dir), rec.LogPath()
	if err := rec.Save(); err != nil {
		fmt.Fprintln(o.Err, err)
		return 1
	}
	if err := agent.SpawnArgvContext(ctx, &rec, backgroundArgs(args, rec.ID), nil); err != nil {
		rec.Finish(wire.Result{}, err, 0)
		if saveErr := rec.Save(); saveErr != nil {
			err = errors.Join(err, saveErr)
		}
		fmt.Fprintln(o.Err, err)
		return 1
	}
	if err := agent.WriteJSONLine(o.Out, struct {
		ID  string `json:"id"`
		Log string `json:"log"`
	}{rec.ID, rec.Log}); err != nil {
		fmt.Fprintln(o.Err, err)
		return 1
	}
	return 0
}

func backgroundArgs(args []string, id string) []string {
	out := append([]string{"agents", "dispatch"}, args...)
	return append(out, "--background=false", "--job", id)
}

// RunJob claims its own pid before execution, then settles the shared record without a parent save race.
func RunJob(ctx context.Context, o Options, id string) int {
	return runJob(ctx, o, id, Run)
}

func runJob(ctx context.Context, o Options, id string, run func(context.Context, Options) int) int {
	rec, err := agent.LoadRecord(id)
	if err != nil {
		fmt.Fprintln(o.Err, err)
		return 1
	}
	if rec.Agent != "waves" || rec.Status != "running" || rec.Request.Prompt != o.Plan {
		fmt.Fprintln(o.Err, "not a running wave job for this plan")
		return 2
	}
	rec.PID, rec.PIDStart, rec.Log = os.Getpid(), "", rec.LogPath()
	if err = rec.Save(); err != nil {
		fmt.Fprintln(o.Err, err)
		return 1
	}
	start := time.Now()
	code := run(ctx, o)
	if ctx.Err() != nil {
		code = 4
	}
	return finishJob(&rec, o, code, time.Since(start))
}

func finishJob(rec *agent.Record, o Options, code int, took time.Duration) int {
	var err error
	if code == 4 {
		err = errors.New("interrupted")
	} else if code != 0 {
		err = fmt.Errorf("exit %d", code)
	}
	rec.Finish(wire.Result{}, err, took)
	if err != nil {
		fmt.Fprintln(o.Err, err)
	}
	if err := rec.Save(); err != nil {
		fmt.Fprintln(o.Err, err)
		return 1
	}
	return code
}
