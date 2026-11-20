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
var claudeIdleLimit = 90 * time.Second

// idleWatch feeds claude's events to an agent.IdleTimeout: paused while a top-level tool runs, re-armed by every event.
type idleWatch struct {
	*agent.IdleTimeout
	pending int // tool_use blocks emitted and not yet answered by a tool_result; read by the parser goroutine only
	// api_retry events since the last real event and the last status; claudeExec reads them after the stream ends
	retries, status int
	rejected        bool // authRetryLimit rejected-login retries ended the attempt early
	start           time.Time
}

// newIdleWatch arms the idle window now; kill (nil inside RunEventStream, which binds it) runs once on expiry.
func newIdleWatch(d time.Duration, kill func()) *idleWatch {
	return &idleWatch{IdleTimeout: agent.NewIdleTimeout("claude", d, kill), start: time.Now()}
}

// stalledErr names the API retries behind a stall, since they are not activity; err is returned as is when there were none.
func (w *idleWatch) stalledErr(err error) error {
	if w != nil && w.rejected {
		return w.rejectedErr()
	}
	if w == nil || w.retries == 0 || !errors.Is(err, agent.ErrStalled) {
		return err
	}
	return fmt.Errorf("claude: no model output: %d API retries (%d) in %s: %w",
		w.retries, w.status, time.Since(w.start).Round(time.Second), err)
}

// rejectedErr is the auth-kind failure of a login the API refused; AuthReason makes the fallback chain mark the account.
func (w *idleWatch) rejectedErr() error {
	msg := fmt.Sprintf("claude: login rejected (%d) after %d retries; sign in with `claude-code databricks login --profile <profile>`"+
		" (or `claude /login` for the subscription row), or finish this task in the main session", w.status, w.retries)
	return &retry.Error{Kind: retry.KindAuth, Agent: "claude", Status: w.status,
		AuthReason: fmt.Sprintf("login rejected (%d)", w.status), Err: errors.New(msg)}
}

// observe records an event: it resets the window unless a tool is running, which pauses it.
func (w *idleWatch) observe(ev claudeEvent, msg anthropic.Message) {
	if w == nil {
		return
	}
	if w.authRetry(ev) {
		return
	}
	w.retries = 0
	if ev.ParentToolUseID == "" {
		switch ev.Type {
		case "result":
			if !ev.IsError {
				w.Delivered() // a child that lingers past its result is killed, not failed
			}
		case "assistant":
			w.pending += countBlocks(msg, "tool_use")
		case "user":
			w.pending = max(w.pending-userAnswers(msg), 0)
		}
	}
	if w.pending > 0 { // a result keeps the window armed: a hang before EOF is still a stall
		w.Pause()
		return
	}
	w.Beat(ev.Type)
}

// authRetry counts a retry of a rejected login, which never recovers; a 429 or 5xx backoff still beats.
func (w *idleWatch) authRetry(ev claudeEvent) bool {
	st := ev.ErrorStatus
	if ev.Type != "system" || ev.Subtype != "api_retry" || st == nil || (*st != 401 && *st != 403) {
		return false
	}
	w.noteRetry(st)
	return true
}

func (w *idleWatch) noteRetry(status *int) {
	w.retries++
	w.status = 0
	if status != nil {
		w.status = *status
	}
	if w.retries >= authRetryLimit && !w.rejected {
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
