package ask

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/serve"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/cli/ui/helptext"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// dispatchMode handles --batch, which ends the run before a single task is built.
func (r *askRun) dispatchMode() (int, bool) {
	f := r.f
	if code, done := r.loopMode(); done {
		return code, true
	}
	if f.batchSrc != "" { // --batch picks once from base flags; a single task checks after its record
		if code, done := r.checkArm(); done {
			return code, true
		}
		return r.runBatch()
	}
	if err := batchOnlyFlags(r.fs); err != nil {
		return r.fail(err, 2)
	}
	return 0, false
}

// batchInputs is what --batch and --loop need from the flags: the pool options and the base request.
func batchInputs(r *askRun) (o batchOpts, base agent.RunOptions, code int, done bool) {
	f := r.f
	if code, done := r.resolveLead(); done { // validates --provider/--profile and sets r.lead for batchBase
		return o, base, code, true
	}
	if err := googleToolsCheck(f); err != nil { // batchBase reads the list without a parse error; check it here
		code, done = r.fail(err, 2)
		return o, base, code, done
	}
	o = r.batchOpts()
	if r.auto != "" {
		o.Reg, _ = providers.Load() // a failed load keeps the batch's shared pick
	}
	ov, err := resolveOverlay(r.ov, f.env, f.keepEnv, false)
	if err != nil {
		code, done = r.fail(err, 2)
		return o, base, code, done
	}
	if code, done := r.export(); done {
		return o, base, code, true
	}
	base, err = r.batchBase()
	if err != nil {
		code, done = r.fail(err, 2)
		return o, base, code, done
	}
	return o, base.ApplyOverlay(r.ctx, ov), 0, false
}

// batchOpts is the pool options the flags give --batch and --loop; TimeoutHeld is read from them, never re-derived.
func (r *askRun) batchOpts() batchOpts {
	f := r.f
	return batchOpts{Jobs: f.jobs, FailFast: f.failFast, Agent: r.ref, Fallback: f.fallback, Native: f.native, Auto: r.auto, Identity: f.as,
		Swap: r.swap, Pending: map[string]int{}, TimeoutHeld: timeoutHeld(r.fs, r.ov)}
}

// runBatch runs --batch to completion; every path ends the run.
func (r *askRun) runBatch() (int, bool) {
	o, base, code, done := batchInputs(r)
	if done {
		return code, true
	}
	return runAskBatch(r.ctx, r.fs, r.f.batchSrc, o, base), true
}

// batchOnlyFlags refuses the first --batch-only flag typed without --batch.
func batchOnlyFlags(fs *flag.FlagSet) error {
	for _, name := range []string{"jobs", "fail-fast"} {
		if helptext.Explicit(fs, name) {
			return fmt.Errorf("--%s needs --batch", name)
		}
	}
	return nil
}

func (r *askRun) batchBase() (agent.RunOptions, error) {
	f := r.f
	googleTools, err := googleToolNames(f.googleTools)
	if err != nil {
		return agent.RunOptions{}, err
	}
	_, values := agent.RunValues(r.ctx, nil)
	return agent.RunOptions{Model: f.model, ModelTyped: f.modelTyped, Provider: f.profile, Account: r.ov.Account, Lead: r.lead, Effort: f.effort, WorkDir: f.workdir, Tools: f.tools, NoTools: f.noTools, HTTPDirect: !f.tools && r.fs != nil && helptext.Explicit(r.fs, "tools"),
		Timeout: f.timeout, TimeoutTyped: f.timeoutTyped, KeepEnv: f.keepEnv, Autonomous: f.autonomous, Fleet: f.fleet, Codemode: f.codemode, CodemodeReason: f.codemodeReason, Runner: cmp.Or(f.runner, r.ov.Runner), GoogleTools: googleTools, EnableMemory: f.enableMemory, Direct: f.direct,
		NoPersist: f.noPersist, SystemPrompt: f.systemPrompt, AppendSystemPrompt: f.appendSystemPrompt,
		Env: f.env, OutputSchema: r.schema, Agents: r.defs, Values: values, Rotated: r.replayed.Rotated, FallbackDefaulted: r.fallbackDefaulted(),
		FenceTyped: r.fenceTyped(), TypedFlags: r.f.cli, InheritedOverlay: r.inherited.Doc()}, nil
}

// execute starts a live session or runs the one task.
func (r *askRun) execute() int {
	f := r.f
	if f.live {
		return startLive(r.ctx, r.state, f.name, f.as, f.format)
	}
	if r.jobRec != nil && r.jobRec.Live {
		return agent.RunHolder(r.jobRec, newClient().Launcher)
	}
	framed, prompt, err := r.readPrompt()
	if err != nil {
		return failAsk(f.format, f.name, err)
	}
	if strings.TrimSpace(prompt) == "" {
		r.fs.Usage()
		printAskFailure(f.format, f.name, errors.New("no task: give a prompt, or \"-\" to read stdin"))
		return 2
	}
	join := func() string { return prompt }
	if framed == nil && !f.dryRun {
		join = r.startRecall(prompt) // overlaps the setup below; joined before request reads the brief
	}
	ctx, stop := signal.NotifyContext(r.ctx, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP) // a kill finalizes the record
	defer stop()
	if framed != nil { // the remote end of `ask --host`: stdin closing means the local side cancelled or vanished
		go func() { _, _ = io.Copy(io.Discard, framed); stop() }()
	}
	f.timeout = askTimeout(r.fs, r.ov, f.timeout, f.retry, f.job, f.tools)
	warnAskMemory(r.ag, f.enableMemory)
	f.debugFile = absDebugFile(f.debugFile)
	req, err := r.request(join())
	if err != nil {
		return failAsk(f.format, f.name, err)
	}
	return r.prepareAndRun(ctx, req)
}

// prepareAndRun arms and probes req, layers the overlay over it, prepares the dispatch, then detaches or runs it.
func (r *askRun) prepareAndRun(ctx context.Context, req agent.RunOptions) int {
	run := r.armRun()
	req = agent.MarkReviewLoop(req)
	if run != nil {
		req = run.probe(req)
	}
	ctx = r.dryCtx(ctx, req.Prompt)
	defer releaseRecover(ctx)
	return r.askPrepare(ctx, req, run)
}

// overlayedReq applies the resolved overlay to req and checks the agent can run it.
func (r *askRun) overlayedReq(ctx context.Context, req agent.RunOptions) (agent.RunOptions, error) {
	f := r.f
	ov, err := resolveOverlay(r.ov, f.env, f.keepEnv, f.background)
	if err != nil {
		return req, err
	}
	req = req.ApplyOverlay(ctx, ov)
	return req, agent.CheckCapabilities(f.name, req)
}

// askPrepare prepares the dispatch, then detaches or runs it and reports.
func (r *askRun) askPrepare(ctx context.Context, req agent.RunOptions, run *armRetry) int {
	f := r.f
	req, err := r.overlayedReq(ctx, req)
	if err != nil {
		return failAskCode(f.format, f.name, err, 2)
	}
	cl := newClient()
	ctx = providers.WithIdentity(ctx, cmp.Or(f.as, providers.IdentityHeadless)) // headless: a paired row tries its SP first
	req.Identity = cmp.Or(f.as, req.Identity)                                   // recorded: replay/reboot keeps a typed --as
	if dry, ok := ctx.Value(dryKey{}).(dryDoc); ok {
		return dryRun(ctx, req, dry, f.format, r.out)
	}
	if err := claimRecover(ctx); err != nil {
		return failAsk(f.format, f.name, err)
	}
	prepared, err := prepareAdmitted(ctx, cl, req, r.jobRec, run)
	if err != nil {
		return failAskCode(f.format, f.name, err, prepareExitCode(err))
	}
	if err := takeClaim(r.jobRec, prepared.Record.ID); err != nil {
		if r.jobRec != nil {
			agent.RecordRefusal(r.jobRec.Agent, req, r.jobRec, err)
		}
		return failAsk(f.format, f.name, err)
	}
	finishRecover(ctx, prepared.Record.ID)
	if err := startAsk(ctx, r.jobRec, &prepared.Record); err != nil {
		return failAsk(f.format, f.name, err)
	}
	markRetryOf(ctx, &prepared.Record, f.background)
	saveErr := startPrepared(cl, prepared, f.background)
	if f.background {
		return spawnHeld(ctx, r.fs, prepared, f.native, f.format, f.name, saveErr)
	}
	if code, unsaved := unsavedHeld(ctx, prepared.Record, f.format, saveErr); unsaved {
		return code
	}
	return runSettled(ctx, cl, prepared, req, run, r.jobRec != nil, f.format, f.save, r.out)
}

// takeClaim clears the inherited dispatch claim and, unless this is a --job, claims it for id.
func takeClaim(jobRec *agent.Record, id string) error {
	claim := os.Getenv(agent.DispatchClaimEnv)
	if err := proc.Unsetenv(agent.DispatchClaimEnv); err != nil {
		return err
	}
	if jobRec != nil {
		return nil
	}
	return agent.ClaimDispatchID(claim, id)
}

// markRetryOf names the retried run on a background retry's record, for its --job child.
func markRetryOf(ctx context.Context, rec *agent.Record, background bool) {
	if r, ok := agent.RetriedOf(ctx); ok && background {
		rec.RetryOf = r.ID
	}
}

// startPrepared prints p's preflight lines and saves its record; a failed save is warned about unless background.
func startPrepared(cl *agent.Client, p *agent.Prepared, background bool) error {
	for _, l := range p.Chain.Preflight.Lines() {
		fmt.Fprintln(os.Stderr, "preflight:", l)
	}
	cl.Notify = quietPrinted(cl.Notify, p.Chain.Preflight)
	err := p.Record.Save()
	if err != nil && !background {
		fmt.Fprintf(os.Stderr, "ask: warning: dispatch record not saved, --retry may not find it: %v\n", err)
	}
	return err
}

// quietPrinted drops the skip entries notify would repeat after pf's preflight: lines; other messages pass.
func quietPrinted(notify func(string), pf *agent.Preflight) func(string) {
	if notify == nil || pf == nil || len(pf.Failed) == 0 {
		return notify
	}
	return func(s string) {
		rest, ok := strings.CutPrefix(s, agent.SkippedLead)
		if !ok {
			notify(s)
			return
		}
		kept := slices.DeleteFunc(strings.Split(rest, "; "), func(e string) bool {
			return slices.ContainsFunc(pf.Failed, func(f agent.Finding) bool { return strings.HasSuffix(e, ": preflight "+f.String()) })
		})
		if len(kept) > 0 {
			notify(agent.SkippedLead + strings.Join(kept, "; "))
		}
	}
}

func warnAskMemory(ag string, enableMemory bool) {
	if s, ok := agent.LookupSchema(ag); enableMemory && ok && s.CLI.Caps.Memory == agent.None {
		fmt.Fprintf(os.Stderr, "ask: --enable-memory has no effect on the %s agent (copilot only)\n", ag)
	}
}

// prepareExitCode is the code a Prepare error names, else 1.
func prepareExitCode(err error) int {
	if _, ok := errors.AsType[*agent.UnacceptedError](err); ok || errors.Is(err, providers.ErrModelSpec) {
		return 2
	}
	if usage, ok := errors.AsType[interface {
		error
		ExitCode() int
	}](err); ok {
		return usage.ExitCode()
	}
	return 1
}

// admitJob is serve.Admit; a test seam.
var admitJob = serve.Admit

// admitPending holds a detached job (jobRec set) until `agents serve` admits its pool; with no daemon it runs at once.
func admitPending(ctx context.Context, jobRec *agent.Record) {
	if jobRec != nil {
		agent.NoteQueued(jobRec.ID, serve.PoolOf(*jobRec), os.Stderr)
		defer agent.NoteQueued(jobRec.ID, "", os.Stderr)
		admitJob(ctx, *jobRec, os.Stderr)
	}
}

func prepareAdmitted(ctx context.Context, cl *agent.Client, req agent.RunOptions, jobRec *agent.Record, run *armRetry) (*agent.Prepared, error) {
	admitPending(ctx, jobRec)
	err := ctx.Err()
	var prepared *agent.Prepared
	if err == nil {
		go warmMask() // resolve the vault key while Prepare runs
		prepared, err = run.prepare(ctx, cl, req, jobRec)
	}
	if err != nil && jobRec != nil {
		agent.RecordRefusal(jobRec.Agent, req, jobRec, err)
	}
	return prepared, err
}

// askSpawn detaches a prepared dispatch as a background job; its child starts from the run values it was prepared with.
func askSpawn(ctx context.Context, fs *flag.FlagSet, prepared *agent.Prepared, native bool, format, name string) int {
	if prepared.Opts.Values != nil {
		ctx = proc.WithValues(ctx, prepared.Opts.Values)
	}
	if err := agent.SpawnContext(ctx, &prepared.Record, backgroundExtraFlags(fs, prepared.Opts.Fallback, native)); err != nil {
		return failAsk(format, name, failSpawn(&prepared.Record, err))
	}
	if err := reportBackgroundStart(format, prepared.Record); err != nil {
		return helptext.Fail("%s: job %s started; write job id: %v", name, prepared.Record.ID, err)
	}
	return 0
}

// stdoutSink is the sink behind stdout for format: streamed text, nothing (json prints one envelope), or NDJSON.
func stdoutSink(format, dispatchID string) wire.Sink { return sinkTo(os.Stdout, format, dispatchID) }

// sinkTo is stdoutSink writing to out.
func sinkTo(out io.Writer, format, dispatchID string) wire.Sink {
	switch format {
	case helptext.JSON:
		return wire.Discard
	case agent.FormatStreamJSON:
		return maskedEvents{wire.NewJSONLSink(out, dispatchID)}
	}
	return wire.NewTextSink(out)
}

// backgroundExtraFlags carries explicit routing and identity flags over to the spawned --job child.
func backgroundExtraFlags(fs *flag.FlagSet, fallback string, native bool) []string {
	var extra []string
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "as":
			extra = append(extra, "--as", f.Value.String())
		case "fallback":
			extra = append(extra, "--fallback", fallback)
		case "native":
			if native {
				extra = append(extra, "--native")
			}
		}
	})
	return extra
}

// reportBackgroundStart tells the caller how to track a detached job.
func reportBackgroundStart(format string, rec agent.Record) error {
	if format == helptext.JSON {
		return json.NewEncoder(os.Stdout).Encode(struct {
			ID  string `json:"id"`
			Log string `json:"log"`
		}{rec.ID, rec.Log})
	}
	_, err := fmt.Printf("job %s started — claude-code agents status|result|cancel %s\n", rec.ID, rec.ID)
	return err
}

// failAsk reports a pre-run failure; with --format json it prints the same envelope a run would.
func failAsk(format, agentName string, err error) int { return failAskCode(format, agentName, err, 1) }

// failAskCode is failAsk exiting with code (2 for a usage error).
func failAskCode(format, agentName string, err error, code int) int {
	printAskFailure(format, agentName, err)
	helptext.Fail("ask: %v", err)
	return code
}

// printAskFailure prints the typed failure envelope on stdout when format is json; a lost write is noted on stderr.
func printAskFailure(format, agentName string, err error) {
	var werr error
	switch format {
	case helptext.JSON:
		werr = writeEnvelope(os.Stdout, wire.FailureEnvelope(agentName, err))
	case agent.FormatStreamJSON:
		werr = stdoutSink(format, "").Emit(wire.ResultEvent(wire.FailureEnvelope(agentName, err)))
	}
	if werr != nil {
		fmt.Fprintf(os.Stderr, "ask: failure envelope not written to stdout: %v\n", werr)
	}
}
