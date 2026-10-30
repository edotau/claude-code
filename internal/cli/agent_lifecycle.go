package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/vendoragent"
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
	format := outfmt.Flag(fs)
	follow := new(bool)
	if verb == "tail" {
		follow = fs.Bool("f", false, "follow until the job finishes, with a heartbeat on silence")
	}
	pos, err := outfmt.Parse(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := outfmt.Check(*format); err != nil {
		return fail("agents %s: %v", verb, err)
	}
	if len(pos) != 1 {
		fs.Usage()
		return 2
	}
	rec, err := vendoragent.Find(pos[0])
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
	rec, err = vendoragent.CancelJob(context.Background(), rec.Agent, cwd, rec.ID)
	if err != nil {
		return fail("agents cancel: %v", err)
	}
	if *format == outfmt.JSON {
		if err := outfmt.Write(os.Stdout, struct {
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
	rows, err := vendoragent.Status(context.Background(), rec.Agent, cwd, rec.ID, true)
	if err != nil {
		return fail("agents status: %v", err)
	}
	if format == outfmt.JSON {
		if err := outfmt.Write(os.Stdout, rows); err != nil {
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
	if format == outfmt.JSON {
		rec, err = vendoragent.TailEvents(ctx, rec, follow, os.Stdout)
	} else {
		rec, err = vendoragent.Tail(ctx, rec.Agent, cwd, rec.ID, follow, os.Stdout)
	}
	if err != nil {
		return fail("agents tail: %v", err)
	}
	return tailExit(rec, follow)
}

func agentsResult(rec agent.Record, format, cwd string) int {
	rec, log, err := vendoragent.Result(context.Background(), rec.Agent, cwd, rec.ID)
	if err != nil {
		return fail("agents result: %v", err)
	}
	if format == outfmt.JSON {
		if err := outfmt.Write(os.Stdout, recordEnvelope(rec)); err != nil {
			return fail("agents result: %v", err)
		}
		return resultExit(rec)
	}
	fmt.Print(log)
	if log != "" && !strings.HasSuffix(log, "\n") {
		fmt.Println()
	}
	if line := recordEnvelope(rec).RanOn; line != "" {
		fmt.Println("ask:", line)
	}
	if rec.SessionID != "" {
		fmt.Printf("session: %s\n", rec.SessionID)
		fmt.Printf("resume: claude-code ask --agent %s --session %s\n", rec.Agent, rec.SessionID)
	}
	return resultExit(rec)
}

// recordEnvelope is the result event's envelope, or one rebuilt from rec when the events file is absent.
func recordEnvelope(rec agent.Record) agent.Envelope {
	if f, err := os.Open(rec.EventsPath()); err == nil {
		defer f.Close()
		var env *agent.Envelope
		sc := bufio.NewScanner(f)
		sc.Buffer(nil, 16<<20)
		for sc.Scan() {
			var ev agent.Event
			if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Type == agent.EventResult && ev.Envelope != nil {
				env = ev.Envelope
			}
		}
		if env != nil {
			return *env
		}
	}
	var runErr error
	if rec.Status == "error" {
		runErr = fmt.Errorf("%s", rec.Error)
	}
	res := agent.Result{Agent: rec.Agent, Provider: rec.Provider, Model: rec.Model, Invoked: rec.Invoked,
		SessionID: rec.SessionID, DurationMs: rec.DurationMs, Fallback: rec.Fallback, FallbackReason: rec.FallbackReason}
	if log, err := os.ReadFile(rec.LogPath()); err == nil {
		res.Answer = string(log)
	}
	return agent.NewEnvelope(res, runErr, rec.ID)
}
