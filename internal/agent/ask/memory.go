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
	"github.com/edotau/claude-code/internal/agentconfig"
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

// memoryRunOptions pins the memory job to claude's own CLI: it holds write tools, so Pi never leads.
func memoryRunOptions(ctx context.Context, prompt, workdir string) agent.RunOptions {
	o := agent.RunOptions{Agent: "claude", Prompt: prompt, Effort: "medium", Tools: memory.JobTools(memory.JobKind(ctx)), WorkDir: workdir,
		Fallback: router.FallbackNone, Runner: agentconfig.RunnerOwn}
	if schema := memory.JobAnswerSchema(ctx); schema != "" {
		o.OutputSchema, o.Tools = schema, false
	}
	return o
}

// answerText is the structured answer when the run produced one, else the plain text.
func answerText(res wire.Result) string {
	if len(res.Structured) > 0 {
		return string(res.Structured)
	}
	return res.Answer
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

// jobClass maps the terminal hop's failure to a typed class and the command that fixes it.
func jobClass(reg *providers.Registry, profile string, err error) (class, fix string) {
	e, _ := errors.AsType[*retry.Error](err)
	switch {
	case errors.Is(err, providers.ErrNeedsLogin) || e != nil && e.Kind == retry.KindAuth:
		if slices.Contains(providers.DatabricksProfiles(reg), profile) {
			return memory.JobClassAuth, "claude-code databricks login --profile " + profile
		}
		return memory.JobClassAuth, "claude-code doctor"
	case errors.Is(err, agent.ErrTreeBudget):
		return memory.JobClassBudget, "claude-code memory update --session " + memory.FixSessionSlot
	case e != nil && (e.Kind == retry.KindQuota || e.Kind == retry.KindRateLimit):
		return memory.JobClassQuota, ""
	}
	return memory.JobClassFailed, ""
}

// memoryHop runs one hop and prints its receipt.
func memoryHop(ctx context.Context, c *agent.Client, opts agent.RunOptions) (wire.Result, error) {
	ctx, stop := memorySignals(ctx)
	start := time.Now()
	note := memory.JobOutput(ctx, os.Stderr)
	fmt.Fprintf(note, "memory job: pid %d started %s\n", os.Getpid(), start.Format(time.RFC3339))
	res, err := c.Run(ctx, opts, memorySink(memory.JobOutput(ctx, os.Stdout)))
	fmt.Fprintln(note, memoryReceipt(os.Getpid(), res.DispatchID, time.Since(start), stop(), err))
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

// planLeadProfile is the workspace a failed Prepare's plan admitted first; empty without one.
func planLeadProfile(plan *agent.RoutePlan) string {
	if plan == nil || len(plan.Admitted) == 0 {
		return ""
	}
	return plan.Admitted[0].Workspace.Profile
}

// hopRun runs one dispatch hop; the real one is memoryHop, tests pass a fake.
type hopRun func(ctx context.Context, opts agent.RunOptions) (wire.Result, error)

// attempt is one hop's outcome with the profile it ran on, kept so the terminal failure is classified, not the pin's.
type attempt struct {
	res     wire.Result
	profile string
	local   bool
	err     error
}

// MemoryRunner runs background memory jobs on Claude on their pin, then the other workspaces, then the local model.
func MemoryRunner() memory.AgentFunc {
	return func(ctx context.Context, prompt, workdir string) (string, error) {
		c := newClient()
		c.Refresh = nil
		c.Notify = func(m string) { fmt.Fprintln(memory.JobOutput(ctx, os.Stderr), m) }
		run := func(ctx context.Context, o agent.RunOptions) (wire.Result, error) { return memoryHop(ctx, c, o) }
		opts := memoryRunOptions(ctx, prompt, workdir)
		opts.Env = memory.ChildEnv(ctx)
		reg, rerr := providers.Load()
		if rerr != nil {
			reg = &providers.Registry{}
		}
		res, err := run(ctx, opts)
		plan, _ := agent.PlanOfError(err)
		first := attempt{res: res, profile: cmp.Or(res.Provider, planLeadProfile(plan), providers.SessionLeadContext(ctx, reg, true)), err: err}
		if err == nil {
			memory.AnswerJob(ctx, answerText(res))
			memory.ReportJob(ctx, memory.JobResult{Agent: res.Agent, Profile: res.Provider, Model: res.Model})
			return memoryWho(res, false), nil
		}
		if rerr != nil || !agent.HopFailed(res, err) {
			reportFailure(ctx, reg, first)
			return memoryWho(res, false), err
		}
		return memoryWalk(ctx, run, reg, opts, first)
	}
}

// hopOptions aims opts at one hop: route and legacy provider agree, and no lead or account from the pin survives.
func hopOptions(opts agent.RunOptions, hop route.Route) agent.RunOptions {
	opts.Route = hop
	opts.Provider = hop.Provider + ":" + hop.Profile
	opts.Lead, opts.Account, opts.CellRoute = hop.Profile, "", route.Route{}
	return opts
}

// reportFailure files the terminal attempt's class, route and trust; an earlier hop's failure is history only.
func reportFailure(ctx context.Context, reg *providers.Registry, a attempt) {
	class, fix := jobClass(reg, a.profile, a.err)
	if class == memory.JobClassBudget {
		fmt.Fprintln(memory.JobOutput(ctx, os.Stderr), "memory job: retry outside the spent dispatch tree, or after raising the cap")
	}
	memory.ReportJob(ctx, memory.JobResult{Agent: cmp.Or(a.res.Agent, "claude"), Profile: a.profile, Model: a.res.Model,
		Local: a.local, Class: class, Fix: fix})
}

// memoryWalk tries each later hop after the pin failed before answering; the first answer or non-hop failure ends it.
func memoryWalk(ctx context.Context, run hopRun, reg *providers.Registry, opts agent.RunOptions, pin attempt) (string, error) {
	last, who, errs := pin, memoryWho(pin.res, false), []error{pin.err}
	for _, hop := range memoryHops(reg, pin.profile) {
		local := hop == localRoute
		if local && !localUp(ctx, reg) {
			fmt.Fprintln(memory.JobOutput(ctx, os.Stderr), "memory job: local model down; skipped")
			break
		}
		res, err := run(ctx, hopOptions(opts, hop))
		who = memoryWho(res, local)
		if err == nil {
			memory.AnswerJob(ctx, answerText(res))
			memory.ReportJob(ctx, memory.JobResult{Agent: res.Agent, Profile: hop.Profile, Model: res.Model, Local: local})
			return who, nil
		}
		last = attempt{res: res, profile: hop.Profile, local: local, err: err}
		errs = append(errs, err)
		if !agent.HopFailed(res, err) {
			break
		}
	}
	reportFailure(ctx, reg, last)
	return who, errors.Join(errs...)
}
