package ask

import (
	"context"
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/router"
)

// memoryRunOptions pins the memory job to claude: it holds write tools, so it never falls back to another vendor.
// Gate: only the job's memory.JobScope is writable and only its listed commands run; plan mode denies the rest.
func memoryRunOptions(ctx context.Context, prompt, workdir string) agent.RunOptions {
	s := memory.JobScope(memory.JobKind(ctx), workdir)
	p := agent.Permissions{Mode: "plan", Deny: []agent.Rule{{Tool: "Bash", Arg: agent.RedirectFileArg}}}
	for _, tool := range []string{"Edit", "Write"} {
		for _, d := range s.NoWrite {
			p.Deny = append(p.Deny, agent.Rule{Tool: tool, Arg: d})
		}
		for _, w := range s.Write {
			p.Allow = append(p.Allow, agent.Rule{Tool: tool, Arg: w})
		}
	}
	for _, dir := range s.CdDirs {
		p.Allow = append(p.Allow, agent.CdRule(dir))
	}
	for _, b := range s.Bash {
		p.Allow = append(p.Allow, agent.Rule{Tool: "Bash", Arg: b})
	}
	return agent.RunOptions{Agent: "claude", Prompt: prompt, Effort: "medium", Tools: s.Tools, WorkDir: workdir,
		Fallback: router.FallbackNone, Gate: true, Permissions: p}
}

// MemoryRunner runs background memory jobs on Claude as the logged-in user, on their pin (medium effort).
func MemoryRunner() memory.AgentFunc {
	return func(ctx context.Context, prompt, workdir string) (string, error) {
		c := newClient()
		c.Refresh = nil
		c.Notify = func(m string) { fmt.Fprintln(os.Stderr, m) }
		opts := memoryRunOptions(ctx, prompt, workdir)
		opts.Env = memory.ChildEnv(ctx)
		res, err := c.Run(ctx, opts, wire.NewTextSink(os.Stdout))
		who := res.Agent
		if res.Fallback != "" {
			who += "<-" + res.Fallback
		}
		memory.NoteGateBlocks(ctx, res.GateBlocks)
		return who, err
	}
}
