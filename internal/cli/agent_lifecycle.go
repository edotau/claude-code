package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// agentsJobPage returns `agents <verb> -h`.
func agentsJobPage(verb string) helptext.Page {
	about := map[string]string{
		"status": "Show one dispatch's state, whichever agent ran it (claude included).",
		"result": "Print a finished dispatch's output; --format json prints its envelope.",
		"tail":   "Print a dispatch's log; --format json streams its events.jsonl instead.",
		"cancel": "Interrupt a running background dispatch, whichever agent runs it.",
	}[verb]
	return helptext.Page{Usage: "claude-code agents " + verb + " <id> [flags]", About: about}
}

// cmdAgentsJob is `claude-code agents status|result|tail|cancel <id>`: the agent comes from the dispatch record.
func cmdAgentsJob(verb string, args []string) int {
	fs := helptext.NewFlagSet("agents "+verb, agentsJobPage(verb))
	format := helptext.FormatFlag(fs)
	follow := new(bool)
	if verb == "tail" {
		follow = fs.Bool("f", false, "follow until the job finishes, with a heartbeat on silence")
	}
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return badFormat("agents "+verb, err)
	}
	if len(pos) != 1 {
		fs.Usage()
		return 2
	}
	rec, err := agent.Find(pos[0])
	if err != nil {
		return fail("agents %s: %v", verb, err)
	}
	cwd, _ := os.Getwd()
	switch verb {
	case "status":
		return agentsStatus(rec, *format, cwd)
	case "result":
		return agentsResult(rec, *format, cwd)
	case "tail":
		return agentsTail(rec, *format, *follow, cwd)
	}
	rec, err = agent.CancelJob(context.Background(), rec.Agent, cwd, rec.ID)
	if err != nil {
		return fail("agents cancel: %v", err)
	}
	if *format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  string `json:"error,omitempty"`
		}{rec.ID, rec.Status, rec.Error}); err != nil {
			return fail("agents cancel: %v", err)
		}
		return 0
	}
	fmt.Printf("cancelled %s\n", rec.ID)
	return 0
}

func agentsStatus(rec agent.Record, format, cwd string) int {
	rows, err := agent.Status(context.Background(), rec.Agent, cwd, rec.ID, true)
	if err != nil {
		return fail("agents status: %v", err)
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, rows); err != nil {
			return fail("agents status: %v", err)
		}
		return 0
	}
	for _, r := range rows {
		fmt.Printf("%-24s %-8s %-10s %s\n", r.ID, r.Status, r.Elapsed, statusDetail(r))
	}
	return 0
}

func agentsTail(rec agent.Record, format string, follow bool, cwd string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	if format == helptext.JSON {
		rec, err = agent.TailEvents(ctx, rec, follow, os.Stdout)
	} else {
		rec, err = agent.Tail(ctx, rec.Agent, cwd, rec.ID, follow, os.Stdout)
	}
	if err != nil {
		return fail("agents tail: %v", err)
	}
	return tailExit(rec, follow)
}

func agentsResult(rec agent.Record, format, cwd string) int {
	rec, log, err := agent.JobResult(context.Background(), rec.Agent, cwd, rec.ID)
	if err != nil {
		return fail("agents result: %v", err)
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, agent.RecordEnvelope(rec, os.Stderr)); err != nil {
			return fail("agents result: %v", err)
		}
		return resultExit(rec)
	}
	fmt.Print(log)
	if log != "" && !strings.HasSuffix(log, "\n") {
		fmt.Println()
	}
	if line := agent.RecordEnvelope(rec, os.Stderr).RanOn; line != "" {
		fmt.Println("ask:", line)
	}
	if rec.SessionID != "" {
		fmt.Printf("session: %s\n", rec.SessionID)
		fmt.Printf("resume: claude-code ask --agent %s --session %s\n", rec.Agent, rec.SessionID)
	}
	return resultExit(rec)
}
