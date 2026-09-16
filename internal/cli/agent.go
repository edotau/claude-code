package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

var errNotImplemented = errors.New("not implemented")

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
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *format != "text" && *format != "json" {
		return fail("ask: --format %q unknown (text|json)", *format)
	}
	prompt := strings.Join(fs.Args(), " ")
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
	res, err := agent.Run(ctx, r, req, stream, func(s string) { fmt.Fprintln(os.Stderr, "ask:", s) })
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
	if err != nil {
		return fail("ask: %v", err)
	}
	return 0
}

func cmdAgents(args []string) int {
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
