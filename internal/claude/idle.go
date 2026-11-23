package claude

import (
	"errors"
	"fmt"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
)

// authRetryLimit is how many consecutive 401/403 api_retry events end an attempt: a rejected login never recovers.
const authRetryLimit = 3

// claudeIdleLimit is how long a claude child may emit nothing while a model response is expected; tests shorten it.
var claudeIdleLimit = agent.IdleLimitFor("claude")

// idleWatch feeds claude's events to an agent.IdleTimeout: paused while a top-level tool runs, re-armed by every event.
type idleWatch struct {
	*agent.IdleTimeout
	pending int // tool_use blocks emitted and not yet answered by a tool_result; read by the parser goroutine only
	// api_retry events since the last real event and the last status; claudeExec reads them after the stream ends
	retries, status int
	authRun         int  // consecutive 401/403 retries; a 429/5xx retry or a real event resets it
	rejected        bool // authRetryLimit rejected-login retries ended the attempt early
	start           time.Time
	tools           map[string]openTool // open top-level tool_use id -> call, each with its own pause ceiling
	idle            time.Duration       // the window length outside a tool call
	paused          bool                // a tool-call ceiling window is running
	pauseEnd        time.Time           // ceiling for pending calls whose ids were not tracked
	row             string              // the launch profile row, named in a rejected-login message; empty when unknown
	onModel         func(string)        // gets the model the stream reports, so a live run is labelled with it
}

type openTool struct {
	name     string
	deadline time.Time
}

// toolPauseCeiling bounds how long one tool call may hold the idle window off; tests shorten it.
var toolPauseCeiling = 10 * time.Minute

// newIdleWatch arms the idle window now; kill (nil inside RunEventStream, which binds it) runs once on expiry.
func newIdleWatch(d time.Duration, kill func()) *idleWatch {
	return &idleWatch{IdleTimeout: agent.NewIdleTimeout("claude", d, kill), start: time.Now(), idle: d, tools: map[string]openTool{}}
}

// stalledErr names the API retries behind a stall, since they are not activity; err is returned as is when there were none.
func (w *idleWatch) stalledErr(err error) error {
	if w != nil && w.rejected {
		return w.rejectedErr()
	}
	if w.ceilingHit(err) {
		return fmt.Errorf("claude: tool %s ran past the %s pause ceiling: %w", w.earliestTool().name, toolPauseCeiling, err)
	}
	if w == nil || w.retries == 0 || !errors.Is(err, agent.ErrStalled) {
		return err
	}
	return fmt.Errorf("claude: no model output: %d API retries (%d) in %s: %w",
		w.retries, w.status, time.Since(w.start).Round(time.Second), err)
}

// ceilingHit reports an expiry while a tool call held the window.
func (w *idleWatch) ceilingHit(err error) bool {
	return w != nil && w.paused && len(w.tools) > 0 && (errors.Is(err, agent.ErrStalled) || errors.Is(err, agent.ErrSilent))
}

// rejectedErr is the auth-kind failure of a login the API refused; AuthReason makes the fallback chain mark the account.
func (w *idleWatch) rejectedErr() error {
	msg := fmt.Sprintf("claude: login rejected (%d) after %d retries; sign in with `claude /login`, or finish this task in the main session",
		w.status, w.retries)
	if w.row != "" {
		msg = fmt.Sprintf("claude: login rejected (%d) after %d retries on %s; sign in with `claude-code databricks login --profile %s`"+
			" if %s is a Databricks profile (`claude /login` for the subscription row), or finish this task in the main session",
			w.status, w.retries, w.row, w.row, w.row)
	}
	return &retry.Error{Kind: retry.KindAuth, Agent: "claude", Status: w.status,
		AuthReason: fmt.Sprintf("login rejected (%d)", w.status), Err: errors.New(msg)}
}

// observe records an event: it resets the window unless a tool is running, which pauses it.
func (w *idleWatch) observe(ev claudeEvent, msg anthropic.Message) {
	if w == nil {
		return
	}
	if ev.Type == "system" && ev.Subtype == "api_retry" { // a failing API call is not progress: never re-arm
		w.noteRetry(ev.ErrorStatus)
		return
	}
	w.retries, w.authRun = 0, 0
	if ev.Model != "" && w.onModel != nil {
		w.onModel(ev.Model)
	}
	if ev.ParentToolUseID == "" {
		switch ev.Type {
		case "result":
			if !ev.IsError {
				w.Delivered() // a child that lingers past its result is killed, not failed
			}
		case "assistant":
			w.pending += countBlocks(msg, "tool_use")
			w.trackTools(msg, false)
		case "user":
			w.pending = max(w.pending-userAnswers(msg), 0)
			w.trackTools(msg, true)
		}
	}
	w.rearm(ev.Type)
}

func (w *idleWatch) noteRetry(status *int) {
	w.retries++
	w.status = 0
	if status != nil {
		w.status = *status
	}
	if status == nil || (*status != 401 && *status != 403) {
		w.authRun = 0
		return
	}
	w.authRun++
	if w.authRun >= authRetryLimit && !w.rejected {
		w.rejected = true
		w.Kill()
	}
}

func countBlocks(msg anthropic.Message, typ string) (n int) {
	for _, b := range msg.Content {
		if b.Type == typ {
			n++
		}
	}
	return n
}

// userAnswers is the tool_results a user event answers; an undecodable one (no blocks) counts as one so pending drains.
func userAnswers(msg anthropic.Message) int {
	if len(msg.Content) == 0 {
		return 1
	}
	return countBlocks(msg, "tool_result")
}

// earliestTool is the open tool call whose ceiling expires first.
func (w *idleWatch) earliestTool() openTool {
	best := openTool{name: "unknown"}
	for _, t := range w.tools {
		if best.deadline.IsZero() || t.deadline.Before(best.deadline) {
			best = t
		}
	}
	return best
}

// trackTools records opened tool calls by id, or drops the answered ones (all once none is pending).
func (w *idleWatch) trackTools(msg anthropic.Message, answered bool) {
	for _, b := range msg.Content {
		if answered {
			delete(w.tools, b.ToolUseID)
		} else if b.Type == "tool_use" {
			w.tools[b.ID] = openTool{name: b.Name, deadline: time.Now().Add(toolPauseCeiling)}
		}
	}
	if w.pending == 0 {
		clear(w.tools)
	}
}

// rearm arms the earliest open tool call's ceiling (later events do not extend it), else beats the idle window.
func (w *idleWatch) rearm(what string) {
	switch {
	case w.pending > 0: // a result keeps the window armed: a hang before EOF is still a stall
		if !w.paused {
			w.paused = true
			w.pauseEnd = time.Now().Add(toolPauseCeiling)
		}
		end := w.pauseEnd
		if t := w.earliestTool(); len(w.tools) > 0 {
			end = t.deadline
		}
		w.Rearm(max(time.Until(end), time.Millisecond), what)
	case w.paused:
		w.paused = false
		w.Rearm(w.idle, what)
	default:
		w.Beat(what)
	}
}
