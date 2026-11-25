package ask

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
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
	if r.jobRec != nil || r.f.retry != "" || !r.f.tools || len(prompt) < recallMinPrompt ||
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
	return agent.RunOptions{Agent: "claude", Prompt: prompt, Effort: "medium", Tools: memory.JobTools(memory.JobKind(ctx)), WorkDir: workdir,
		Fallback: router.FallbackNone}
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

// localRoute is the always-on final hop: Qwen on Magnitude, reached through the proxy's translation.
var localRoute = route.Route{Provider: "magnitude", Profile: "local"}

const localProbe = 2 * time.Second

// memoryHops is the ordered list after the pin: the other Databricks workspaces, then the local model.
func memoryHops(reg *providers.Registry, ran string) []route.Route {
	var out []route.Route
	if slices.Contains(providers.DatabricksProfiles(reg), ran) {
		for _, ws := range router.WorkspaceRows(reg, ran) {
			out = append(out, route.Route{Provider: providers.GroupDatabricks, Profile: ws})
		}
	}
	return append(out, localRoute)
}

// localUp asks the local row's /models within localProbe and wants its sonnet model listed.
func localUp(ctx context.Context, reg *providers.Registry) bool {
	row := reg.Row(localRoute.Profile)
	if row == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, localProbe)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(row.BaseURL, "/")+"/models", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode == http.StatusOK && strings.Contains(string(body), row.Models["sonnet"])
}

// jobClass maps the pin hop's failure to a typed class and the command that fixes it.
func jobClass(reg *providers.Registry, profile string, err error) (class, fix string) {
	e, _ := errors.AsType[*retry.Error](err)
	switch {
	case errors.Is(err, providers.ErrNeedsLogin) || e != nil && e.Kind == retry.KindAuth:
		if slices.Contains(providers.DatabricksProfiles(reg), profile) {
			return memory.JobClassAuth, "claude-code databricks login --profile " + profile
		}
		return memory.JobClassAuth, "claude-code doctor"
	case errors.Is(err, agent.ErrTreeBudget):
		return memory.JobClassBudget, "claude-code memory update --catchup"
	case e != nil && (e.Kind == retry.KindQuota || e.Kind == retry.KindRateLimit):
		return memory.JobClassQuota, ""
	}
	return memory.JobClassFailed, ""
}

// memoryHop runs one hop and prints its receipt.
func memoryHop(ctx context.Context, c *agent.Client, opts agent.RunOptions) (wire.Result, error) {
	ctx, stop := memorySignals(ctx)
	start := time.Now()
	fmt.Fprintf(os.Stderr, "memory job: pid %d started %s\n", os.Getpid(), start.Format(time.RFC3339))
	res, err := c.Run(ctx, opts, memorySink(os.Stdout))
	fmt.Fprintln(os.Stderr, memoryReceipt(os.Getpid(), res.DispatchID, time.Since(start), stop(), err))
	return res, err
}

func memoryWho(res wire.Result, local bool) string {
	who := res.Agent
	if res.Fallback != "" {
		who += "<-" + res.Fallback
	}
	if local {
		who += "·local"
	}
	return who
}

// MemoryRunner runs background memory jobs on Claude on their pin, then the other workspaces, then the local model.
func MemoryRunner() memory.AgentFunc {
	return func(ctx context.Context, prompt, workdir string) (string, error) {
		c := newClient()
		c.Refresh = nil
		c.Notify = func(m string) { fmt.Fprintln(os.Stderr, m) }
		opts := memoryRunOptions(ctx, prompt, workdir)
		opts.Env = memory.ChildEnv(ctx)
		res, err := memoryHop(ctx, c, opts)
		if err == nil || !agent.HopFailed(res, err) {
			if err == nil {
				memory.ReportJob(ctx, memory.JobResult{Agent: res.Agent, Profile: res.Provider, Model: res.Model})
			}
			return memoryWho(res, false), err
		}
		return memoryWalk(ctx, c, opts, res, err)
	}
}

// memoryWalk tries each later hop after the pin failed before answering; the first answer or non-hop failure ends it.
func memoryWalk(ctx context.Context, c *agent.Client, opts agent.RunOptions, pin wire.Result, pinErr error) (string, error) {
	reg, rerr := providers.Load()
	if rerr != nil {
		return memoryWho(pin, false), pinErr
	}
	lastErr, who := pinErr, memoryWho(pin, false)
	for _, hop := range memoryHops(reg, pin.Provider) {
		local := hop == localRoute
		if local && !localUp(ctx, reg) {
			fmt.Fprintln(os.Stderr, "memory job: local model down; skipped")
			break
		}
		opts.Route = hop
		res, err := memoryHop(ctx, c, opts)
		who = memoryWho(res, local)
		if err == nil {
			memory.ReportJob(ctx, memory.JobResult{Agent: res.Agent, Profile: res.Provider, Model: res.Model, Local: local})
			return who, nil
		}
		lastErr = err
		if !agent.HopFailed(res, err) {
			break
		}
	}
	class, fix := jobClass(reg, pin.Provider, pinErr)
	memory.ReportJob(ctx, memory.JobResult{Agent: pin.Agent, Profile: pin.Provider, Class: class, Fix: fix})
	return who, errors.Join(pinErr, lastErr)
}
