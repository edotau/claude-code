package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"

	"github.com/edotau/claude-code/internal/providers"
)

const (
	LiveSchema     = 1
	LiveSockSuffix = ".sock"
)

// LiveRequest is the one line a client sends on the session socket: a single user turn.
type LiveRequest struct {
	Schema int    `json:"schema"`
	Prompt string `json:"prompt"`
}

// LiveBackend is how one agent holds a session open: build the process, write a turn, parse one turn's events.
type LiveBackend interface {
	// Build returns the unstarted child and the provider profile it routes through.
	Build(ctx context.Context, req RunOptions) (cmd *exec.Cmd, tail *TailBuffer, provider string, err error)
	WriteTurn(w io.Writer, prompt string) error
	// Parse reads dec up to and including the turn's result event; usage and cost are that turn's alone.
	Parse(dec *bufio.Reader, sink Sink, lg func(kind, text string)) (Result, error)
}

// LiveCapable is a CLI that can hold a session open; a fresh backend serves each session.
type LiveCapable interface{ NewLive() LiveBackend }

// Live asserts on the CLI behind the agent: the engine itself, or a cliEngine's engine.
func (s Schema) Live() (LiveCapable, bool) {
	r, err := Lookup(s.Name)
	if err != nil {
		return nil, false
	}
	var cli any = r
	if v, ok := r.(cliEngine); ok {
		cli = v.HeadlessDriver
	}
	lc, ok := cli.(LiveCapable)
	return lc, ok
}

// HasLive reports whether the agent can hold a live session.
func (s Schema) HasLive() bool {
	_, ok := s.Live()
	return ok
}

func (claudeEngine) NewLive() LiveBackend { return &claudeLive{} }

// claudeLive holds `claude --print --input-format stream-json`: one process, one user message per turn.
type claudeLive struct{ prevCost float64 }

func (*claudeLive) Build(ctx context.Context, req RunOptions) (*exec.Cmd, *TailBuffer, string, error) {
	args := claudeArgs([]string{"--print", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--include-partial-messages"}, req)
	cmd, tail, cl, err := claudeCmd(ctx, req, args, "")
	return cmd, tail, cl.Set[providers.EnvSessionProfile], err
}

func (*claudeLive) WriteTurn(w io.Writer, prompt string) error {
	return writeJSONLine(w, map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": prompt}})
}

// parse prices the turn alone: claude's total_cost_usd accumulates over the process, its token counts do not.
func (b *claudeLive) Parse(dec *bufio.Reader, sink Sink, lg func(kind, text string)) (Result, error) {
	res, err := parseClaudeEvents(dec, true, sink, lg)
	total := res.Usage.CostUSD
	res.Usage.CostUSD = max(total-b.prevCost, 0)
	b.prevCost = max(b.prevCost, total)
	return res, err
}

// writeJSONLine writes v as one NDJSON line.
func writeJSONLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// The helpers below are the live package's reach into the dispatch internals.

// ChildErr names a failed child by its stderr tail; a deadline surfaces as ctx.Err.
func ChildErr(ctx context.Context, name string, err error, tail *TailBuffer) error {
	return childErr(ctx, name, err, tail)
}

// StatusOf maps a classified failure to the envelope status.
func StatusOf(e *Error) string { return statusOf(e) }

// EventLogFor binds ctx's dispatch log; nil when the dispatch has none.
func EventLogFor(ctx context.Context) func(kind, text string) { return eventLogFor(ctx) }

// NewRunSink stamps agent's name on sink's events and counts its text bytes.
func NewRunSink(sink Sink, agent string) Sink { return &runSink{Sink: sink, agent: agent} }

// IsResultError reports a failed turn whose result event was consumed, so the stream sits at a turn boundary.
func IsResultError(err error) bool {
	_, ok := errors.AsType[resultError](err)
	return ok
}
