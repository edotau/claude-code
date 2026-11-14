package ask

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/router"
)

// memoryRunOptions pins the memory job to claude: it holds write tools, so it never falls back to another vendor.
// Gate: only the job's bank globs are writable and only its listed commands run; plan mode denies the rest.
func memoryRunOptions(ctx context.Context, prompt, workdir string) agent.RunOptions {
	write, bash := memory.BankScope(ctx, workdir)
	p := agent.Permissions{Mode: "plan", Deny: []agent.Rule{{Tool: "Bash", Arg: agent.RedirectFileArg}}}
	for _, tool := range []string{"Edit", "Write"} {
		p.Deny = append(p.Deny, agent.Rule{Tool: tool, Arg: memory.StoreDir() + "/.git/**"})
		for _, w := range write {
			p.Allow = append(p.Allow, agent.Rule{Tool: tool, Arg: w})
		}
	}
	for _, w := range write { // cd only into a bank directory
		if dir, ok := strings.CutSuffix(w, "/**"); ok {
			p.Allow = append(p.Allow, agent.CdRule(dir))
		}
	}
	for _, b := range bash {
		p.Allow = append(p.Allow, agent.Rule{Tool: "Bash", Arg: b})
	}
	return agent.RunOptions{Agent: "claude", Prompt: prompt, Effort: "medium", Tools: true, WorkDir: workdir,
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
