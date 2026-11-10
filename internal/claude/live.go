package claude

import (
	"bufio"
	"context"
	"io"
	"os/exec"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
)

func (claudeEngine) NewLive() agent.LiveBackend { return &claudeLive{} }

// claudeLive holds `claude --print --input-format stream-json`: one process, one user message per turn.
type claudeLive struct {
	prevCost float64
	cmd      *exec.Cmd // the held child, killed by a turn's stall watch
}

func (b *claudeLive) Build(ctx context.Context, req agent.RunOptions) (*exec.Cmd, *retry.TailBuffer, string, error) {
	args := claudeArgs([]string{"--print", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--include-partial-messages"}, req, anthropicLaunch(req))
	cmd, tail, cl, err := claudeCmd(ctx, req, args, "")
	b.cmd = cmd
	return cmd, tail, cl.Set[providers.EnvSessionProfile], err
}

func (*claudeLive) WriteTurn(w io.Writer, prompt string) error {
	return agent.WriteJSONLine(w, map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": prompt}})
}

// parse prices the turn alone: claude's total_cost_usd accumulates over the process, its token counts do not.
// A silent child is killed after claudeIdleLimit, so a turn cannot stay busy forever.
func (b *claudeLive) Parse(dec *bufio.Reader, sink wire.Sink, lg func(kind, text string)) (wire.Result, error) {
	wd := newStallWatch(claudeIdleLimit, func() { _ = b.cmd.Cancel() })
	defer wd.stop()
	res, err := parseClaudeEvents(dec, true, wd, sink, lg)
	if serr := wd.stalled(); serr != nil {
		err = serr
	}
	total := res.Usage.CostUSD
	res.Usage.CostUSD = max(total-b.prevCost, 0)
	b.prevCost = max(b.prevCost, total)
	return res, err
}
