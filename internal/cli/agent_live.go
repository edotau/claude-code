package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/vendoragent"
)

// liveReadyWait bounds how long `ask --live` waits for its holder to open the socket.
const liveReadyWait = time.Minute

// askLiveCheck rejects what a live session cannot honor; the live-holder cap applies to a new session only (job == "").
func askLiveCheck(name, job, batch, retry string, attach, env int) error {
	switch {
	case !agent.HasLive(name):
		return fmt.Errorf("--live: the %s agent has no live mode (claude, gemini)", name)
	case batch != "" || retry != "":
		return errors.New("--live cannot be combined with --batch or --retry")
	case attach > 0:
		return errors.New("--live cannot take --attach: send the file's text in a turn")
	case env > 0:
		return errors.New("--env cannot be combined with --live: env values are never stored in the dispatch record")
	}
	if job != "" {
		return nil
	}
	if live := agent.LiveRecords(); len(live) >= agent.MaxLive {
		ids := make([]string, len(live))
		for i, r := range live {
			ids[i] = r.ID
		}
		return fmt.Errorf("%d live sessions already running (max %d): %s; end one with claude-code agents cancel <id>",
			len(live), agent.MaxLive, strings.Join(ids, ", "))
	}
	return nil
}

// runAskLive is the detached holder: it runs the session as one recorded dispatch until idle, cancel or exit.
func runAskLive(ctx context.Context, rec *agent.Record, req agent.Request, idle time.Duration) int {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	notify := func(s string) { fmt.Fprintln(os.Stderr, "ask:", s) }
	_, err := agent.Dispatch(ctx, rec, rec.Agent, agent.Discard, agent.DispatchOpts{Job: true, Notify: notify},
		func(ctx context.Context, sink agent.Sink) (agent.Result, error) {
			return agent.RunLive(ctx, rec, req, agent.LiveOpts{Idle: idle}, sink)
		})
	if err != nil {
		return fail("ask: %v", err)
	}
	return 0
}

// reportLiveStart waits for the holder's socket, then tells the caller how to drive the session.
func reportLiveStart(format string, rec agent.Record) int {
	deadline := time.Now().Add(liveReadyWait)
	for {
		if _, err := os.Stat(agent.SocketPath(rec.ID)); err == nil {
			break
		}
		if !proc.Alive(rec.PID) || time.Now().After(deadline) {
			why := "did not open its socket in time"
			if latest, err := agent.LoadRecord(rec.ID); err == nil && latest.Error != "" {
				why = latest.Error
			} else if !proc.Alive(rec.PID) {
				why = "exited before opening its socket"
			}
			return failAsk(format, rec.Agent, fmt.Errorf("live session %s: %s (log %s)", rec.ID, why, rec.LogPath()))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if format == formatJSON {
		body, _ := json.Marshal(struct {
			ID     string `json:"id"`
			Log    string `json:"log"`
			Socket string `json:"socket"`
			PID    int    `json:"pid"`
		}{rec.ID, rec.Log, agent.SocketPath(rec.ID), rec.PID})
		fmt.Println(string(body))
		return 0
	}
	fmt.Printf("live %s (pid %d) — claude-code agents send %s \"<task>\"; agents cancel %s ends it\n", rec.ID, rec.PID, rec.ID, rec.ID)
	return 0
}

// agentsSendPage is `agents send -h`.
var agentsSendPage = helptext.Page{
	Usage: "claude-code agents send [flags] <id> <task | ->",
	About: "Send one user turn to a live session (ask --live), stream that turn's events and exit on its result. " +
		"One turn runs at a time; a second send is refused. With no task, or \"-\", the task is read from stdin.",
}

// cmdAgentsSend is `claude-code agents send <id> <task>`.
func cmdAgentsSend(args []string) int {
	fs := helptext.NewFlagSet("agents send", agentsSendPage)
	format := fs.String("format", "text", "stream text, print the turn's JSON envelope, or stream-json NDJSON events")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() < 1 || (*format != formatText && *format != formatJSON && *format != formatStreamJSON) {
		fs.Usage()
		return 2
	}
	prompt, err := resolveAskPrompt(strings.Join(fs.Args()[1:], " "))
	if err != nil {
		return fail("agents send: %v", err)
	}
	if strings.TrimSpace(prompt) == "" {
		return fail("agents send: no task: give a prompt, or \"-\" to read stdin")
	}
	rec, err := vendoragent.Find(fs.Arg(0))
	if err != nil {
		return fail("agents send: %v", err)
	}
	if !rec.Live {
		return fail("agents send: dispatch %s is not a live session (start one with ask --live)", rec.ID)
	}
	if !rec.Alive() {
		return fail("agents send: live session %s has ended (%s)", rec.ID, rec.Status)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	tw := &lastByteWriter{w: os.Stdout}
	text := agent.NewTextSink(tw)
	res, err := agent.SendTurn(ctx, rec.ID, prompt, func(line []byte, ev agent.Event) error {
		switch *format {
		case formatStreamJSON:
			_, err := os.Stdout.Write(line)
			return err
		case formatText:
			return text.Emit(ev)
		}
		return nil
	})
	if err != nil {
		return fail("agents send: %v", err)
	}
	return reportTurn(*format, res, tw.last != '\n' && tw.last != 0)
}

// reportTurn prints the turn's closing output and returns its exit code.
func reportTurn(format string, res agent.Event, endLine bool) int {
	env := res.Envelope
	switch format {
	case formatJSON:
		if env != nil {
			if err := outfmt.Write(os.Stdout, env); err != nil {
				return fail("agents send: %v", err)
			}
		}
	case formatText:
		if endLine {
			fmt.Println()
		}
		if env != nil && env.Usage != nil {
			fmt.Fprintf(os.Stderr, "turn %d: %d in, %d out, $%.4f\n", res.Turn, env.Usage.InputTokens, env.Usage.OutputTokens, env.Usage.CostUSD)
		}
	}
	if env == nil || env.Status != agent.StatusOK {
		if env != nil && env.Error != nil {
			fmt.Fprintln(os.Stderr, "agents send:", env.Error.Message)
		}
		return 1
	}
	return 0
}

// lastByteWriter remembers the last byte written, so the closing newline is added only when the text lacks one.
type lastByteWriter struct {
	w    io.Writer
	last byte
}

func (l *lastByteWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		l.last = p[len(p)-1]
	}
	return l.w.Write(p)
}
