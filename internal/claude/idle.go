package claude

import (
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/edotau/claude-code/internal/agent"
)

// claudeIdleLimit is how long a claude child may emit nothing while a model response is expected; tests shorten it.
var claudeIdleLimit = 90 * time.Second

// idleWatch feeds claude's events to an agent.IdleTimeout: paused while a top-level tool runs, re-armed by every event.
type idleWatch struct {
	*agent.IdleTimeout
	pending int // tool_use blocks emitted and not yet answered by a tool_result; read by the parser goroutine only
}

// newIdleWatch arms the idle window now; kill (nil inside RunEventStream, which binds it) runs once on expiry.
func newIdleWatch(d time.Duration, kill func()) *idleWatch {
	return &idleWatch{IdleTimeout: agent.NewIdleTimeout("claude", d, kill)}
}

// observe records an event: it resets the window unless a tool is running, which pauses it.
func (w *idleWatch) observe(ev claudeEvent, msg anthropic.Message) {
	if w == nil {
		return
	}
	if ev.ParentToolUseID == "" {
		switch ev.Type {
		case "assistant":
			w.pending += countBlocks(msg, "tool_use")
		case "user":
			w.pending = max(w.pending-max(countBlocks(msg, "tool_result"), 1), 0)
		}
	}
	if w.pending > 0 { // a result keeps the window armed: a hang before EOF is still a stall
		w.Pause()
		return
	}
	w.Beat(ev.Type)
}

// countBlocks counts msg's content blocks of the given type.
func countBlocks(msg anthropic.Message, typ string) (n int) {
	for _, b := range msg.Content {
		if b.Type == typ {
			n++
		}
	}
	return n
}
