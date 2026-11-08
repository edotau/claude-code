package agent

import (
	"context"
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/memory"
)

// MemoryRunner runs background memory jobs on Claude as the logged-in user, on their pin (medium effort).
func MemoryRunner(newClient func() *Client) memory.AgentFunc {
	return func(ctx context.Context, prompt, workdir string) (string, error) {
		client := newClient()
		client.Refresh = nil
		client.Notify = func(m string) { fmt.Fprintln(os.Stderr, m) }
		res, err := client.Run(ctx, RunOptions{Agent: "claude", Prompt: prompt,
			Effort: "medium", Tools: true, WorkDir: workdir}, wire.NewTextSink(os.Stdout))
		who := res.Agent
		if res.Fallback != "" {
			who += "<-" + res.Fallback
		}
		return who, err
	}
}
