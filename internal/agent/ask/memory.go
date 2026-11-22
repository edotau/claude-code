package ask

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
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

// memorySignals cancels ctx on a catchable kill so the run finalizes its record; stop names the signal, if any.
func memorySignals(parent context.Context) (context.Context, func() os.Signal) {
	ctx, cancel := context.WithCancel(parent)
	sigs, got := make(chan os.Signal, 1), make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		select {
		case s := <-sigs:
			got <- s
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() os.Signal {
		signal.Stop(sigs)
		cancel()
		select {
		case s := <-got:
			return s
		default:
			return nil
		}
	}
}

// memoryReceipt is the worker log's end line; a started line with none after it means an uncatchable kill.
func memoryReceipt(pid int, id string, took time.Duration, sig os.Signal, err error) string {
	outcome := "ok"
	if err != nil {
		outcome = "error: " + err.Error()
	}
	if sig != nil {
		outcome += " (signal " + sig.String() + ")"
	}
	return fmt.Sprintf("memory job: pid %d dispatch %s ended after %s: %s", pid, cmp.Or(id, "-"), took.Round(time.Millisecond), outcome)
}

// MemoryRunner runs background memory jobs on Claude as the logged-in user, on their pin (medium effort).
func MemoryRunner() memory.AgentFunc {
	return func(ctx context.Context, prompt, workdir string) (string, error) {
		c := newClient()
		c.Refresh = nil
		c.Notify = func(m string) { fmt.Fprintln(os.Stderr, m) }
		ctx, stop := memorySignals(ctx)
		opts := memoryRunOptions(ctx, prompt, workdir)
		opts.Env = memory.ChildEnv(ctx)
		start := time.Now()
		fmt.Fprintf(os.Stderr, "memory job: pid %d started %s\n", os.Getpid(), start.Format(time.RFC3339))
		res, err := c.Run(ctx, opts, memorySink(os.Stdout))
		fmt.Fprintln(os.Stderr, memoryReceipt(os.Getpid(), res.DispatchID, time.Since(start), stop(), err))
		who := res.Agent
		if res.Fallback != "" {
			who += "<-" + res.Fallback
		}
		memory.NoteGateBlocks(ctx, res.GateBlocks)
		return who, err
	}
}
