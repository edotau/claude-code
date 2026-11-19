package ask

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/router"
)

// startRecall begins the memory-recall lookup and returns a join that gives the brief with its block appended once;
// a replay, --safe-mode or a held block sends the brief as is.
func (r *askRun) startRecall(prompt string) func() string {
	if r.jobRec != nil || r.f.retry != "" || r.f.safeMode {
		return func() string { return prompt }
	}
	done := make(chan string, 1)
	go func() {
		session := os.Getenv(memory.SessionIDEnv)
		block, _ := memory.AgentRecall(r.ctx, session, prompt)
		memory.LogBriefRecall(session, block)
		done <- block
	}()
	return func() string { return prompt + <-done }
}

// memoryRunOptions pins the memory job to claude: it holds write tools, so it never falls back to another vendor.
func memoryRunOptions(ctx context.Context, prompt, workdir string) agent.RunOptions {
	s := memory.JobScope(memory.JobKind(ctx), workdir)
	p := agent.Permissions{Mode: "plan", Deny: []agent.Rule{{Tool: "Bash", Arg: agent.RedirectFileArg}}}
	for _, secret := range []string{"**/state/vault.db", "**/.credentials.json", "**/.env", "**/*.pem"} {
		p.Deny = append(p.Deny, agent.Rule{Tool: "Read", Arg: secret}) // also binds read-only Bash
	}
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
	for _, b := range s.NoBash {
		p.Deny = append(p.Deny, agent.Rule{Tool: "Bash", Arg: b})
	}
	// read-only inspection; echo is the job's separator, and the redirect deny still refuses `echo x > f`
	p.Allow = append(p.Allow, agent.ReadOnlyAllow, agent.Rule{Tool: "Bash", Arg: "echo *"})
	return agent.RunOptions{Agent: "claude", Prompt: prompt, Effort: "medium", Tools: s.Tools, WorkDir: workdir,
		Fallback: router.FallbackNone, Gate: true, DataPrompt: true, Permissions: p}
}

// memorySink prints a memory job's text events masked; the job reads the files it wrote, not this text.
func memorySink(w io.Writer) wire.Sink { return maskedEvents{wire.NewTextSink(w)} }

// MemoryRunner runs background memory jobs on Claude as the logged-in user, on their pin (medium effort).
func MemoryRunner() memory.AgentFunc {
	return func(ctx context.Context, prompt, workdir string) (string, error) {
		c := newClient()
		c.Refresh = nil
		c.Notify = func(m string) { fmt.Fprintln(os.Stderr, m) }
		opts := memoryRunOptions(ctx, prompt, workdir)
		opts.Env = memory.ChildEnv(ctx)
		res, err := c.Run(ctx, opts, memorySink(os.Stdout))
		who := res.Agent
		if res.Fallback != "" {
			who += "<-" + res.Fallback
		}
		memory.NoteGateBlocks(ctx, res.GateBlocks)
		return who, err
	}
}
