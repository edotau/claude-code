package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/agent/explore"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/agent/roles"
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

func Cmd(args []string) int {
	fs := helptext.NewFlagSet("dispatch", dispatchPage)
	var parts partFlag
	fs.Var(&parts, "part", "explore: one sub-question per leg (repeatable, max 12)")
	noWeb := fs.Bool("no-web", false, "explore: code scan only, skip web research")
	models := fs.String("models", "", "compare: model ids, comma-separated (default: the role's set)")
	jobs := fs.Int("jobs", explore.MaxParts, "legs running at once (max 12)")
	timeout := fs.Duration("timeout", 0, "per-leg time limit (0 = the agent's default)")
	format := helptext.FormatFlag(fs)
	background := fs.Bool("background", false, "detach each leg and print its id; no merged report")
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
	if len(parts) > explore.MaxParts {
		return helptext.FailUsage("dispatch: %d --part values; the fan-out cap is %d", len(parts), explore.MaxParts)
	}
	role, err := roles.Lookup(pos[0])
	if err != nil {
		return helptext.FailUsage("dispatch: %v", err)
	}
	cwd, _ := os.Getwd()
	q := roles.Request{Task: strings.Join(pos[1:], " "), WorkDir: cwd, Parts: parts, Web: !*noWeb, Timeout: *timeout}
	for m := range strings.SplitSeq(*models, ",") {
		if m = strings.TrimSpace(m); m != "" {
			q.Models = append(q.Models, m)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return dispatchRun(ctx, role, q, *jobs, *format, *background)
}

// dispatchRun builds the legs (splitting explore when no --part) and runs or detaches them.
func dispatchRun(ctx context.Context, role roles.Role, q roles.Request, jobs int, format string, background bool) int {
	client := ask.NewClient()
	if role.Name == "explore" && len(q.Parts) == 0 && !background {
		q.Parts = explore.Split(ctx, client, q.Task)
	}
	legs := roles.Legs(role, q)
	if len(legs) == 0 {
		return helptext.FailUsage("dispatch: role %q builds no legs (a renamed role must be explore, fleet or compare)", role.Name)
	}
	if background {
		return dispatchDetach(ctx, client, legs, format)
	}
	var progress io.Writer = os.Stderr
	if format == helptext.JSON {
		progress = nil
	}
	rep := roles.Run(ctx, client, role, legs, max(1, jobs), progress)
	if format == helptext.JSON {
		body, _ := json.Marshal(rep)
		fmt.Println(string(body))
	} else {
		rep.Text(os.Stdout)
	}
	if rep.Status == "error" {
		return 1
	}
	return 0
}

// dispatchDetach spawns each leg as `ask --background` does and prints its id (one JSON line per leg with --format json).
func dispatchDetach(ctx context.Context, client *agent.Client, legs []roles.Leg, format string) int {
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
			err = job.Spawn(&prepared.Record, nil)
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
			fmt.Println(string(body))
			continue
		}
		fmt.Printf("%s\t", leg.Label)
		ask.ReportBackgroundStart(helptext.Text, prepared.Record)
	}
	return code
}
