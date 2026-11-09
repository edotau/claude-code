package agentcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent/client"

	"github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/agent/roles"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// dispatchPage is `dispatch -h`.
var dispatchPage = helptext.Page{
	Usage: "claude-code agents dispatch <role> [flags] <task>",
	About: "Run a task by role: explore (gemini, one leg per --part), fleet (copilot fleet mode) " +
		"or compare (opencode, one leg per model). Legs run concurrently; the report lists each leg's answer.",
	Hint: "Roles come from the vault overlay dispatch-roles, else the built-in table.",
}

// partFlag collects repeatable --part entries.
type partFlag []string

func (p *partFlag) String() string { return strings.Join(*p, ",") }

// Placeholder names the FLAGS placeholder for partFlag (helptext.Placeholder).
func (p *partFlag) Placeholder() string { return "QUESTION" }

// Set appends one sub-question.
func (p *partFlag) Set(s string) error {
	*p = append(*p, s)
	return nil
}

// dispatchCmd is `agents dispatch`.
func dispatchCmd(args []string) int {
	fs := helptext.NewFlagSet("dispatch", dispatchPage)
	var parts partFlag
	fs.Var(&parts, "part", "explore: one sub-question per leg (repeatable, max 12)")
	noWeb := fs.Bool("no-web", false, "explore: code scan only, skip web research")
	models := fs.String("models", "", "compare: model ids, comma-separated (default: the role's set)")
	jobs := fs.Int("jobs", roles.MaxParts, "legs running at once (max 12)")
	timeout := fs.Duration("timeout", 0, "per-leg time limit (0 = the agent's default)")
	format := helptext.FormatFlag(fs)
	background := fs.Bool("background", false, "detach each leg and print its id; no merged report")
	outputSchema := fs.String("output-schema", "", "JSON Schema (file or inline) each answer must satisfy")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("dispatch", err)
	}
	if len(pos) < 2 {
		fs.Usage()
		return 2
	}
	role, schema, err := resolveInputs(pos[0], len(parts), *outputSchema)
	if err != nil {
		return helptext.FailUsage("dispatch: %v", err)
	}
	cwd, _ := os.Getwd()
	q := roles.Request{Task: strings.Join(pos[1:], " "), WorkDir: cwd, Parts: parts, Models: splitModels(*models),
		Web: !*noWeb, Timeout: *timeout, OutputSchema: schema}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return dispatchRun(ctx, role, q, *jobs, *format, *background)
}

// resolveInputs checks the part cap, looks up the role and loads the --output-schema, before any leg runs.
func resolveInputs(roleName string, nparts int, schemaArg string) (roles.Role, string, error) {
	if nparts > roles.MaxParts {
		return roles.Role{}, "", fmt.Errorf("%d --part values; the fan-out cap is %d", nparts, roles.MaxParts)
	}
	role, err := roles.Lookup(roleName)
	if err != nil {
		return roles.Role{}, "", err
	}
	schema, err := client.ResolveOutputSchema(schemaArg)
	return role, schema, err
}

// splitModels returns the non-empty, trimmed entries of a comma-separated --models value.
func splitModels(s string) []string {
	var out []string
	for m := range strings.SplitSeq(s, ",") {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out
}

// dispatchRun builds the legs (splitting explore when no --part) and runs or detaches them.
func dispatchRun(ctx context.Context, role roles.Role, q roles.Request, jobs int, format string, background bool) int {
	cl := ask.NewClient()
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

// errWriter keeps the first write error, so output whose callee drops write errors still surfaces it.
type errWriter struct {
	w   io.Writer
	err error
}

// Write forwards to w and records the first failure.
func (e *errWriter) Write(p []byte) (int, error) {
	n, err := e.w.Write(p)
	if err != nil && e.err == nil {
		e.err = err
	}
	return n, err
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
	out := &errWriter{w: os.Stdout}
	if err := rep.Text(out); err != nil {
		return err
	}
	return out.err
}

// dispatchDetach spawns each leg as `ask --background` does and prints its id (one JSON line per leg with --format json).
func dispatchDetach(ctx context.Context, client *client.Client, legs []roles.Leg, format string) int {
	code := 0
	for _, leg := range legs {
		if leg.Err != "" {
			code = helptext.Fail("dispatch: %s: %s", leg.Label, leg.Err)
			continue
		}
		leg.Opts.Background = true // the saved record must not carry this short-lived process's pid
		prepared, err := client.Prepare(ctx, leg.Opts, nil)
		if err == nil {
			err = prepared.Record.Save()
		}
		if err == nil {
			if err = job.Spawn(&prepared.Record, nil); err != nil {
				prepared.Record.Finish(wire.Result{}, err, time.Since(prepared.Record.Time))
				_ = prepared.Record.Save()
			}
		}
		if err != nil {
			code = helptext.Fail("dispatch: %s: %v", leg.Label, err)
			continue
		}
		if format == helptext.JSON {
			body, _ := json.Marshal(struct {
				Label string `json:"label"`
				ID    string `json:"id"`
				Log   string `json:"log"`
			}{leg.Label, prepared.Record.ID, prepared.Record.Log})
			if _, err := fmt.Fprintln(os.Stdout, string(body)); err != nil {
				code = helptext.Fail("dispatch: %s: write job id: %v", leg.Label, err)
			}
			continue
		}
		fmt.Printf("%s\t", leg.Label)
		ask.ReportBackgroundStart(helptext.Text, prepared.Record)
	}
	return code
}
