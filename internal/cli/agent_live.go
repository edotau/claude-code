package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// liveExclusive are the ask flags a live session cannot honor: it has no single task, route or envelope of its own.
var liveExclusive = []string{"background", "host", "batch", "retry", "job", "env", "attach", "no-tools", "output-schema",
	"rubric", "log", "stdin-framed", "fallback", "native"}

// askLiveCheck rejects --live with another agent, a task, or a flag that has no meaning for a held-open session.
func askLiveCheck(fs *flag.FlagSet, name string) error {
	if !agent.HasLive(name) {
		return fmt.Errorf("--live is claude or gemini only, not %s", name)
	}
	if fs.NArg() > 0 {
		return errors.New("--live takes no task: start it, then claude-code agents send <id> <task>")
	}
	for _, f := range liveExclusive {
		if flagExplicit(fs, f) {
			return fmt.Errorf("--live cannot be combined with --%s", f)
		}
	}
	return nil
}

func startLive(s *askState, name, format string) int {
	r, err := agent.Lookup(name)
	if err == nil {
		err = r.Available()
	}
	if err == nil {
		err = agent.CheckLiveCap("")
	}
	if err != nil {
		return failAsk(format, name, err)
	}
	req := agent.Request{Model: *s.model, Provider: *s.provider, Effort: *s.effort, WorkDir: *s.workdir, SessionID: *s.session,
		Tools: *s.tools, Timeout: *s.timeout, KeepEnv: []string(*s.keepEnv), NoPersist: *s.noPersist, SafeMode: *s.safeMode,
		DebugFile: absDebugFile(*s.debugFile), SystemPrompt: *s.systemPrompt, AppendSystemPrompt: *s.appendSystemPrompt,
		Autonomous: *s.autonomous, Direct: *s.direct, Launcher: launch.Launcher{}}
	rec := agent.BuildAskRecord(context.Background(), r.Name(), req, nil, *s.workdir, true)
	rec.Live = true
	if err := rec.Save(); err != nil {
		return failAsk(format, name, err)
	}
	if err := agent.Spawn(&rec, nil); err != nil {
		return failAsk(format, name, err)
	}
	if err := agent.WaitLiveReady(rec.ID, rec.PID); err != nil {
		return failAsk(format, name, fmt.Errorf("live session %s did not start: %w (log: %s)", rec.ID, err, rec.Log))
	}
	if format == formatJSON {
		body, _ := json.Marshal(struct {
			ID     string `json:"id"`
			Socket string `json:"socket"`
			Log    string `json:"log"`
		}{rec.ID, agent.LiveSocket(rec.ID), rec.Log})
		fmt.Println(string(body))
		return 0
	}
	fmt.Printf("live session %s — claude-code agents send %s \"<task>\" · agents cancel %s · idle limit %s\n", rec.ID, rec.ID, rec.ID, agent.IdleOf(req))
	return 0
}

// runLiveHolder is the --job child of `ask --live`: it serves rec's session until idle, cancel or the agent's exit.
func runLiveHolder(rec *agent.Record) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	notify := func(s string) { fmt.Fprintln(os.Stderr, "ask:", s) }
	if err := agent.ServeLive(ctx, rec, notify); err != nil {
		rec.Status, rec.Error = "error", err.Error()
		_ = rec.Save()
		return fail("ask --live: %v", err)
	}
	return 0
}

// agentsSendPage is `agents send -h`.
var agentsSendPage = helptext.Page{
	Usage: "claude-code agents send <id> <task | -> [flags]",
	About: "Send one user turn to a live claude or gemini session (ask --agent <a> --live) and stream " +
		"that turn's events; exits on its result. One turn at a time per session.",
}

// cmdAgentsSend is `claude-code agents send <id> <task>`.
func cmdAgentsSend(args []string) int {
	fs := helptext.NewFlagSet("agents send", agentsSendPage)
	format := fs.String("format", formatText, "stream text, print the JSON envelope, or stream-json NDJSON events")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) < 2 || !slices.Contains([]string{formatText, formatJSON, formatStreamJSON}, *format) {
		fs.Usage()
		return 2
	}
	prompt, err := resolveAskPrompt(strings.Join(pos[1:], " "))
	if err != nil {
		return fail("agents send: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	id := pos[0]
	if rec, ferr := agent.Find(id); ferr == nil {
		id = rec.ID
	}
	env, err := agent.SendLive(ctx, id, prompt, askStdoutSink(*format, id))
	if err != nil {
		return fail("agents send: %v", err)
	}
	if *format == formatJSON {
		body, _ := json.Marshal(env)
		fmt.Println(string(body))
	} else if *format == formatText && env.Answer != "" && !strings.HasSuffix(env.Answer, "\n") {
		fmt.Println()
	}
	if env.Status != agent.StatusOK {
		return 1
	}
	return 0
}
