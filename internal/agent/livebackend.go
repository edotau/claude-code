package agent

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// liveBackend is how one agent holds a session open: build the process, write a turn, parse one turn's events.
type liveBackend interface {
	// build returns the unstarted child and the provider profile it routes through.
	build(ctx context.Context, req RunOptions) (cmd *exec.Cmd, tail *TailBuffer, provider string, err error)
	writeTurn(w io.Writer, prompt string) error
	// parse reads dec up to and including the turn's result event; usage and cost are that turn's alone.
	parse(dec *bufio.Reader, sink Sink, lg eventLog) (Result, error)
}

// LiveCapable is a CLI that can hold a session open; a fresh backend serves each session.
type LiveCapable interface{ newLive() liveBackend }

// Live asserts on the CLI behind the agent's runner: the runner itself, or a cliRunner's engine.
func (s Schema) Live() (LiveCapable, bool) {
	r, err := Lookup(s.Name)
	if err != nil {
		return nil, false
	}
	var cli any = r
	if v, ok := r.(cliRunner); ok {
		cli = v.eng
	}
	lc, ok := cli.(LiveCapable)
	return lc, ok
}

// HasLive reports whether the agent can hold a live session.
func (s Schema) HasLive() bool {
	_, ok := s.Live()
	return ok
}

func (claudeRunner) newLive() liveBackend { return &claudeLive{} }

// claudeLive holds `claude --print --input-format stream-json`: one process, one user message per turn.
type claudeLive struct{ prevCost float64 }

func (*claudeLive) build(ctx context.Context, req RunOptions) (*exec.Cmd, *TailBuffer, string, error) {
	args := claudeArgs([]string{"--print", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--include-partial-messages"}, req)
	cmd, tail, cl, err := claudeCmd(ctx, req, args, "")
	return cmd, tail, cl.Set[providers.PinProfile], err
}

func (*claudeLive) writeTurn(w io.Writer, prompt string) error {
	return writeJSONLine(w, map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": prompt}})
}

// parse prices the turn alone: claude's total_cost_usd accumulates over the process, its token counts do not.
func (b *claudeLive) parse(dec *bufio.Reader, sink Sink, lg eventLog) (Result, error) {
	res, err := parseClaudeEvents(dec, true, sink, lg)
	total := res.Usage.CostUSD
	res.Usage.CostUSD = max(total-b.prevCost, 0)
	b.prevCost = max(b.prevCost, total)
	return res, err
}

// agyLive holds `agy --input-format stream-json`: one conversation, one NDJSON line per turn.
type agyLive struct {
	tools   bool
	intro   string // emulated system prompts, owed to the first turn only
	resumed bool   // a --session conversation: agy's cumulative usage already holds its history
	prev    Usage
}

func (b *agyLive) build(ctx context.Context, req RunOptions) (*exec.Cmd, *TailBuffer, string, error) {
	ctx = directCtx(ctx, req)
	req.Prompt = cmp.Or(req.Prompt, "live session") // prepare wants a prompt; the turns carry the real ones
	p, err := newVendorRunner("gemini").prepare(ctx, req)
	if err != nil {
		return nil, nil, "", err
	}
	areq := p.agyRequest(req)
	bin, err := paths.VendorBin("agy")
	if err != nil {
		return nil, nil, "", fmt.Errorf("gemini: %w", err)
	}
	b.tools, b.resumed = areq.Tools, req.SessionID != ""
	if req.SystemPrompt != "" { // agy has no system-prompt flag: the one-shot path prepends it, so does the first turn
		b.intro += preamble(req.SystemPrompt)
	}
	if req.AppendSystemPrompt != "" {
		b.intro += preamble(req.AppendSystemPrompt)
	}
	cmd, tail := childCmd(ctx, bin, agyArgv(areq), areq.Env, areq.Cwd, "", areq.KeepEnv, true)
	return cmd, tail, p.res.Provider, nil
}

func (b *agyLive) writeTurn(w io.Writer, prompt string) error {
	prompt, b.intro = b.intro+prompt, ""
	line, err := agyStdin(agyRequest{Prompt: prompt, Tools: b.tools})
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, line)
	return err
}

// parse subtracts the usage already reported: agy's result usage is cumulative over the conversation.
func (b *agyLive) parse(dec *bufio.Reader, sink Sink, lg eventLog) (Result, error) {
	res, err := parseAgyEvents(dec, sink, true, lg)
	total := res.Usage
	if b.resumed { // the history behind a --session is unknown: this turn reports no usage, later ones their delta
		b.resumed, b.prev, res.Usage = false, total, Usage{}
		return res, err
	}
	res.Usage = Usage{InputTokens: max(total.InputTokens-b.prev.InputTokens, 0), OutputTokens: max(total.OutputTokens-b.prev.OutputTokens, 0),
		CacheReadTokens: max(total.CacheReadTokens-b.prev.CacheReadTokens, 0), CostUSD: max(total.CostUSD-b.prev.CostUSD, 0)}
	b.prev = total
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
