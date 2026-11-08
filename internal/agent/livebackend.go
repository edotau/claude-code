package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"

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
	Build(ctx context.Context, req RunOptions) (cmd *exec.Cmd, tail *retry.TailBuffer, provider string, err error)
	WriteTurn(w io.Writer, prompt string) error
	// Parse reads dec up to and including the turn's result event; usage and cost are that turn's alone.
	Parse(dec *bufio.Reader, sink wire.Sink, lg func(kind, text string)) (wire.Result, error)
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

// WriteJSONLine writes v as one NDJSON line.
func WriteJSONLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// The helpers below are the live package's reach into the dispatch internals.

// ChildErr names a failed child by its stderr tail; a deadline surfaces as ctx.Err.
func ChildErr(ctx context.Context, name string, err error, tail *retry.TailBuffer) error {
	return childErr(ctx, name, err, tail)
}

// StatusOf maps a classified failure to the envelope status.
func StatusOf(e *retry.Error) string { return statusOf(e) }

// NewRunSink stamps agent's name on sink's events and counts its text bytes.
func NewRunSink(sink wire.Sink, agent string) wire.Sink {
	return &wire.RunSink{Sink: sink, Agent: agent}
}

// IsResultError reports a failed turn whose result event was consumed, so the stream sits at a turn boundary.
func IsResultError(err error) bool {
	_, ok := errors.AsType[wire.ResultError](err)
	return ok
}

// ResolveRequestTarget is the registry and target req's provider and model resolve to.
func ResolveRequestTarget(req RunOptions) (*providers.Registry, providers.Target, error) {
	return resolveTarget(req)
}

// PersonaArgs is the claude argv for a persona.
func PersonaArgs(p *Schema) []string { return personaArgs(p) }

// SessionProvider is the session profile agentName runs on.
func SessionProvider(agentName string) string { return sessionProvider(agentName) }

// CustomEnv is the vault env bucket resolved, as a launch layers it.
func CustomEnv() map[string]string { return customEnv() }
