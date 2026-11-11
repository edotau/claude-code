package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/roles"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// DispatchOptions are `agents dispatch`'s run flags: the role, --output-schema, --jobs, --format and --background.
type DispatchOptions struct {
	Role, Schema, Format string
	Jobs                 int
	Background           bool
}

// Dispatch is `agents dispatch`: resolve the role and --output-schema, then run or detach the legs.
func Dispatch(q roles.Request, o DispatchOptions) int {
	if len(q.Parts) > roles.MaxParts {
		return helptext.FailUsage("dispatch: %d --part values; the fan-out cap is %d", len(q.Parts), roles.MaxParts)
	}
	role, err := roles.Lookup(o.Role)
	if err != nil {
		return helptext.FailUsage("dispatch: %v", err)
	}
	if q.OutputSchema, err = resolveOutputSchema(o.Schema); err != nil {
		return helptext.FailUsage("dispatch: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return dispatchRun(ctx, role, q, o.Jobs, o.Format, o.Background)
}

// dispatchRun builds the legs (splitting explore when no --part) and runs or detaches them.
func dispatchRun(ctx context.Context, role roles.Role, q roles.Request, jobs int, format string, background bool) int {
	cl := newClient()
	if role.Name == "explore" && len(q.Parts) == 0 && !background {
		q.Parts = roles.Split(ctx, cl, q.Task)
	}
	legs := roles.Legs(role, q)
	if len(legs) == 0 {
		return helptext.FailUsage("dispatch: role %q builds no legs (a renamed role must be explore, fleet or compare)", role.Name)
	}
	if background {
		return dispatchDetach(ctx, cl, legs, format)
	}
	var progress io.Writer = os.Stderr
	if format == helptext.JSON {
		progress = nil
	}
	rep := roles.Run(ctx, cl, role, legs, max(1, jobs), progress)
	if err := writeReport(rep, format); err != nil {
		return helptext.Fail("dispatch: write report: %v", err)
	}
	if rep.Status == "error" {
		return 1
	}
	return 0
}

// writeReport prints rep to stdout as one JSON line or as text, returning any write failure.
func writeReport(rep roles.Report, format string) error {
	if format == helptext.JSON {
		body, err := json.Marshal(rep)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(os.Stdout, string(body))
		return err
	}
	return rep.Text(os.Stdout)
}

// dispatchDetach spawns each leg as `ask --background` does and prints its id (one JSON line per leg with json).
func dispatchDetach(ctx context.Context, client *agent.Client, legs []roles.Leg, format string) int {
	code := 0
	for _, leg := range legs {
		if leg.Err != "" {
			code = helptext.Fail("dispatch: %s: %s", leg.Label, leg.Err)
			continue
		}
		rec, err := spawnLeg(ctx, client, leg)
		if err != nil {
			code = helptext.Fail("dispatch: %s: %v", leg.Label, err)
			continue
		}
		if err := printLegID(leg.Label, rec, format); err != nil {
			code = helptext.Fail("dispatch: %s: write job id: %v", leg.Label, err)
		}
	}
	return code
}

// spawnLeg saves and detaches one leg; a failed spawn finishes its record so it never shows as running.
func spawnLeg(ctx context.Context, client *agent.Client, leg roles.Leg) (agent.Record, error) {
	leg.Opts.Background = true // the saved record must not carry this short-lived process's pid
	prepared, err := client.Prepare(ctx, leg.Opts, nil)
	if err != nil {
		return agent.Record{}, err
	}
	if err := prepared.Record.Save(); err != nil {
		return agent.Record{}, err
	}
	if err := agent.Spawn(&prepared.Record, nil); err != nil {
		return agent.Record{}, failSpawn(&prepared.Record, err)
	}
	return prepared.Record, nil
}

// failSpawn finishes a record whose spawn failed so it never shows as running; a failed save joins the returned error.
func failSpawn(rec *agent.Record, err error) error {
	rec.Finish(wire.Result{}, err, time.Since(rec.Time))
	if saveErr := rec.Save(); saveErr != nil {
		err = errors.Join(err, fmt.Errorf("save failed dispatch %s: %w", rec.ID, saveErr))
	}
	return err
}

// printLegID prints a detached leg's id: one JSON line, or the label and ask's background-start text.
func printLegID(label string, rec agent.Record, format string) error {
	if format == helptext.JSON {
		body, _ := json.Marshal(struct {
			Label string `json:"label"`
			ID    string `json:"id"`
			Log   string `json:"log"`
		}{label, rec.ID, rec.Log})
		_, err := fmt.Fprintln(os.Stdout, string(body))
		return err
	}
	if _, err := fmt.Printf("%s\t", label); err != nil {
		return err
	}
	return reportBackgroundStart(helptext.Text, rec)
}
