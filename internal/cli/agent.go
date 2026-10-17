package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/crossgen"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/secretshape"
)

func cmdAsk(args []string) int {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	name := fs.String("agent", "claude", "runner: claude|api|codex|gemini|opencode|copilot")
	provider := fs.String("provider", "", "provider (default: pin, then registry default)")
	model := fs.String("model", "", "slot name, model id, or provider:model")
	effort := fs.String("effort", "", "reasoning effort: low|medium|high|xhigh|max")
	workdir := fs.String("workdir", "", "working directory for the agent")
	session := fs.String("session", "", "resume this session id")
	tools := fs.Bool("tools", false, "allow tool use where the agent can gate it (default: disabled)")
	timeout := fs.Duration("timeout", 10*time.Minute, "per-attempt deadline")
	format := fs.String("format", "text", "text streams the answer; json prints the Result envelope")
	rubric := fs.String("rubric", "", "review standard file quoted verbatim ahead of the task")
	retry := fs.String("retry", "", "replay a logged dispatch by id (or unique id prefix); see --log")
	showLog := fs.Bool("log", false, "list the 20 most recent dispatches and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *format != "text" && *format != "json" {
		return fail("ask: --format %q unknown (text|json)", *format)
	}
	if *showLog {
		return printDispatchLog(*format)
	}
	prompt := strings.Join(fs.Args(), " ")
	if *retry != "" {
		rec, err := agent.LoadRecord(*retry)
		if err != nil {
			return fail("ask: %v", err)
		}
		// The logged request wins; only flags typed on this command line override it.
		*name, *provider, *model, *effort = rec.Agent, rec.Request.Provider, rec.Request.Model, rec.Request.Effort
		*workdir, *session, *tools, *timeout = rec.Request.WorkDir, rec.Request.SessionID, rec.Request.Tools, rec.Request.Timeout
		_ = fs.Parse(args) // re-apply explicit flags over the record
		prompt, *rubric = rec.Request.Prompt, ""
		fmt.Fprintf(os.Stderr, "ask: retrying %s (%s, was %s)\n", rec.ID, rec.Agent, rec.Status)
	}
	if prompt == "" || prompt == "-" {
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fail("ask: read stdin: %v", err)
		}
		prompt = string(raw)
	}
	if strings.TrimSpace(prompt) == "" {
		fmt.Fprintln(os.Stderr, "usage: claude-code ask [flags] <prompt | ->")
		fs.PrintDefaults()
		return 2
	}
	r, err := agent.Lookup(*name)
	if err != nil {
		return fail("ask: %v", err)
	}
	if *rubric != "" {
		if prompt, err = agent.RubricPrompt(*rubric, prompt); err != nil {
			return fail("ask: %v", err)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var stream io.Writer = os.Stdout
	if *format == "json" {
		stream = io.Discard
	}
	req := agent.Request{Prompt: prompt, Model: *model, Provider: *provider, Effort: *effort,
		WorkDir: *workdir, SessionID: *session, Tools: *tools, Timeout: *timeout}
	rec := agent.NewRecord(r.Name(), req, time.Now())
	_ = rec.Save() // fail-open: the log is for replay and forensics, never a gate on the run
	start := time.Now()
	res, err := agent.Run(ctx, r, req, stream, func(s string) { fmt.Fprintln(os.Stderr, "ask:", s) })
	rec.Finish(res, err, time.Since(start))
	_ = rec.Save()
	for _, d := range res.Dropped {
		fmt.Fprintf(os.Stderr, "ask: %s has no channel on the %s agent — dropped\n", d, r.Name())
	}
	if *format == "json" {
		env := struct {
			agent.Result
			Error string `json:"error,omitempty"`
		}{Result: res}
		if err != nil {
			env.Error = err.Error()
		}
		body, _ := json.Marshal(env)
		fmt.Println(string(body))
	} else if res.Answer != "" && !strings.HasSuffix(res.Answer, "\n") {
		fmt.Println()
	}
	fmt.Fprintln(os.Stderr, "ask: dispatch", rec.ID, "(claude-code ask --retry <id> replays it)")
	if err != nil {
		return fail("ask: %v", err)
	}
	return 0
}

const agentsUsage = `usage: claude-code agents
       claude-code agents sync --gemini [--dry-run] [--check] [--no-prune]
       claude-code agents docs [--check] [--dry-run]`

func cmdAgents(args []string) int {
	if len(args) == 0 {
		return listRunners()
	}
	switch args[0] {
	case "sync":
		return cmdAgentsSync(args[1:])
	case "docs":
		return cmdAgentsDocs(args[1:])
	case "-h", "--help":
		fmt.Fprintln(os.Stderr, agentsUsage)
		return 2
	}
	return fail("agents: unknown subcommand %q\n%s", args[0], agentsUsage)
}

// cmdAgentsSync projects agents/ + skills/ into ~/.gemini/skills; --gemini is the only target and is required.
func cmdAgentsSync(args []string) int {
	fs := flag.NewFlagSet("agents sync", flag.ContinueOnError)
	gemini := fs.Bool("gemini", false, "project into ~/.gemini/skills (required)")
	dryRun := fs.Bool("dry-run", false, "report without writing")
	check := fs.Bool("check", false, "dry-run; exit 1 when the projection is stale")
	noPrune := fs.Bool("no-prune", false, "keep orphaned projections")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, agentsUsage)
		return 2
	}
	if !*gemini {
		fmt.Fprintln(os.Stderr, "agents sync: only --gemini is supported")
		return 2
	}
	return crossgen.Sync(crossgen.SyncOptions{DryRun: *dryRun, Check: *check, NoPrune: *noPrune, Out: os.Stdout})
}

// cmdAgentsDocs renders AGENTS.md + GEMINI.md from CLAUDE.md and the agent roster.
func cmdAgentsDocs(args []string) int {
	fs := flag.NewFlagSet("agents docs", flag.ContinueOnError)
	check := fs.Bool("check", false, "exit 1 when a generated file drifted; never writes")
	dryRun := fs.Bool("dry-run", false, "preview without writing")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, agentsUsage)
		return 2
	}
	return crossgen.RenderDocs(crossgen.DocsOptions{Check: *check, DryRun: *dryRun, Out: os.Stdout})
}

func listRunners() int {
	for _, r := range agent.Runners() {
		mark, reason := "✓", ""
		if err := r.Available(); err != nil {
			mark, reason = "✗", err.Error()
		}
		c := r.Caps()
		var caps []string
		for _, f := range []struct {
			ok   bool
			name string
		}{{c.Model, "model"}, {c.Provider, "provider"}, {c.Effort, "effort"}, {c.WorkDir, "workdir"}, {c.SessionID, "session"}, {c.Tools, "tools"}} {
			if f.ok {
				caps = append(caps, f.name)
			}
		}
		fmt.Printf("%s %-9s %-44s %s\n", mark, r.Name(), strings.Join(caps, ","), reason)
	}
	return 0
}

// printDispatchLog lists recent dispatches; the prompt preview is scrubbed of credential shapes.
func printDispatchLog(format string) int {
	recs := agent.RecentRecords(20)
	if format == "json" {
		_ = outfmt.Write(os.Stdout, recs)
		return 0
	}
	if len(recs) == 0 {
		fmt.Println("no dispatches logged yet")
		return 0
	}
	for _, r := range recs {
		preview := strings.Join(strings.Fields(r.Request.Prompt), " ")
		for _, w := range strings.Fields(preview) {
			if secretshape.Looks(w) {
				preview = strings.ReplaceAll(preview, w, "REDACTED")
			}
		}
		if len(preview) > 60 {
			preview = preview[:60] + "…"
		}
		tools := ""
		if r.Request.Tools {
			tools = " tools"
		}
		fmt.Printf("%-28s %-7s %-8s %-9s%s  %s\n", r.ID, r.Status, r.Agent, r.Duration, tools, preview)
	}
	return 0
}
