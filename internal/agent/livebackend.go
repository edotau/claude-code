package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// liveChild is one running agent process a live session feeds turns to.
type liveChild struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	dec   *json.Decoder
	tail  *TailBuffer
	res   Result // provider and model, as the start resolved them
}

// liveBackend is how one agent holds a session open: start the process, write a turn, parse one turn's events.
type liveBackend interface {
	start(ctx context.Context, req Request) (*liveChild, error)
	writeTurn(w io.Writer, prompt string) error
	// parse reads dec up to and including the turn's result event; usage and cost are that turn's alone.
	parse(dec *json.Decoder, sink Sink, lg eventLog) (Result, error)
}

// liveBackendFor is a fresh backend for the agent name, or nil when the agent has no live mode.
func liveBackendFor(name string) liveBackend {
	switch name {
	case "claude":
		return &claudeLive{}
	case "gemini":
		return &agyLive{}
	}
	return nil
}

// HasLive reports whether the named agent can hold a live session.
func HasLive(name string) bool { return liveBackendFor(name) != nil }

// pipeChild starts cmd with a stdin pipe and a stdout decoder.
func pipeChild(cmd *exec.Cmd, tail *TailBuffer) (*liveChild, error) {
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &liveChild{cmd: cmd, stdin: in, dec: json.NewDecoder(out), tail: tail}, nil
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

// claudeLive holds `claude --print --input-format stream-json`: one process, one user message per turn.
type claudeLive struct{ prevCost float64 }

func (*claudeLive) start(ctx context.Context, req Request) (*liveChild, error) {
	args := claudeArgs(req, []string{"--print", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"})
	cmd, tail, plan, err := claudeChild(ctx, req, args, "")
	if err != nil {
		return nil, err
	}
	c, err := pipeChild(cmd, tail)
	if err != nil {
		return nil, err
	}
	c.res.Provider = plan.Set[providers.PinProfile]
	return c, nil
}

func (*claudeLive) writeTurn(w io.Writer, prompt string) error {
	return writeJSONLine(w, map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": prompt}})
}

// parse prices the turn alone: claude's total_cost_usd accumulates over the session, its token counts do not.
func (b *claudeLive) parse(dec *json.Decoder, sink Sink, lg eventLog) (Result, error) {
	res, err := parseClaudeEvents(dec, sink, true, lg)
	total := res.Usage.CostUSD
	res.Usage.CostUSD = max(total-b.prevCost, 0)
	b.prevCost = max(b.prevCost, total)
	return res, err
}

// agyLive holds `agy --input-format stream-json`: one conversation, one NDJSON line per turn.
type agyLive struct {
	tools bool
	prev  Usage
}

func (b *agyLive) start(ctx context.Context, req Request) (*liveChild, error) {
	ctx = directCtx(ctx, req)
	req.Prompt = cmp.Or(req.Prompt, "live session") // prepare wants a prompt; the turns carry the real ones
	p, err := vendorRunner{"gemini"}.prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	areq := p.agyRequest(req)
	bin, err := paths.VendorBin("agy")
	if err != nil {
		return nil, fmt.Errorf("gemini: %w", err)
	}
	b.tools = areq.Tools
	cmd, tail := childCmd(ctx, bin, agyArgv(areq), areq.Env, areq.Cwd, "", areq.KeepEnv, true)
	c, err := pipeChild(cmd, tail)
	if err != nil {
		return nil, err
	}
	c.res.Provider, c.res.Model = p.res.Provider, areq.Model
	return c, nil
}

func (b *agyLive) writeTurn(w io.Writer, prompt string) error {
	line, err := agyStdin(AgyRequest{Prompt: prompt, Tools: b.tools})
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, line)
	return err
}

// parse subtracts the usage already reported: agy's result usage is cumulative over the conversation.
func (b *agyLive) parse(dec *json.Decoder, sink Sink, lg eventLog) (Result, error) {
	res, err := parseAgyEvents(dec, sink, true, lg)
	total := res.Usage
	res.Usage = Usage{InputTokens: max(total.InputTokens-b.prev.InputTokens, 0), OutputTokens: max(total.OutputTokens-b.prev.OutputTokens, 0),
		CacheReadTokens: max(total.CacheReadTokens-b.prev.CacheReadTokens, 0), CostUSD: max(total.CostUSD-b.prev.CostUSD, 0)}
	b.prev = total
	return res, err
}
