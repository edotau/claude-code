package ask

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/permission"
	"github.com/edotau/claude-code/internal/router"
)

const (
	recallMinPrompt = 200 // a shorter prompt is a quick question, not a brief
	recallWait      = 2 * time.Second
)

// recallQuery is the brief's Task: section when it has one, else the whole prompt.
func recallQuery(prompt string) string {
	_, task, ok := strings.Cut(prompt, "\nTask:\n")
	if !ok {
		return prompt
	}
	task, _, _ = strings.Cut(task, "\n## Leaf rules")
	return task
}

// startRecall looks up memory unless skipped and returns a join that appends the block at most once.
func (r *askRun) startRecall(prompt string) func() string {
	if r.jobRec != nil || r.f.retry != "" || r.f.safeMode || !r.f.tools || len(prompt) < recallMinPrompt ||
		strings.Contains(prompt, "<memory-recall>") {
		return func() string { return prompt }
	}
	done := make(chan string, 1)
	ctx, cancel := context.WithCancel(r.ctx)
	go func() {
		defer cancel()
		session := os.Getenv(paths.SessionIDEnv)
		block, _ := memory.AgentRecall(ctx, session, recallQuery(prompt))
		memory.LogBriefRecall(session, block)
		done <- block
	}()
	return func() string {
		select {
		case block := <-done:
			return prompt + block
		case <-time.After(recallWait):
			cancel()
			return prompt
		}
	}
}

// memoryRunOptions pins the memory job to claude: it holds write tools, so it never falls back to another vendor.
func memoryRunOptions(ctx context.Context, prompt, workdir string) agent.RunOptions {
	s := memory.JobScope(memory.JobKind(ctx), workdir)
	p := permission.Permissions{Mode: "plan", Deny: []permission.Rule{{Tool: "Bash", Arg: permission.RedirectFileArg}}}
	for _, secret := range []string{"**/state/vault.db", "**/.credentials.json", "**/.env", "**/*.pem"} {
		p.Deny = append(p.Deny, permission.Rule{Tool: "Read", Arg: secret}) // also binds read-only Bash
	}
	for _, tool := range []string{"Edit", "Write"} {
		for _, d := range s.NoWrite {
			p.Deny = append(p.Deny, permission.Rule{Tool: tool, Arg: d})
		}
		for _, w := range s.Write {
			p.Allow = append(p.Allow, permission.Rule{Tool: tool, Arg: w})
		}
	}
	for _, dir := range s.CdDirs {
		p.Allow = append(p.Allow, permission.CdRule(dir))
	}
	for _, b := range s.Bash {
		p.Allow = append(p.Allow, permission.Rule{Tool: "Bash", Arg: b})
	}
	for _, b := range s.NoBash {
		p.Deny = append(p.Deny, permission.Rule{Tool: "Bash", Arg: b})
	}
	// read-only inspection; echo is the job's separator, and the redirect deny still refuses `echo x > f`
	p.Allow = append(p.Allow, permission.ReadOnlyAllow, permission.Rule{Tool: "Bash", Arg: "echo *"})
	return agent.RunOptions{Agent: "claude", Prompt: prompt, Effort: "medium", Tools: s.Tools, WorkDir: workdir,
		Fallback: router.FallbackNone, Gate: true, DataPrompt: true, Maintenance: true, Permissions: p}
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
