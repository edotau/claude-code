package ask

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// carryTail bounds the partial output a Pi takeover is given, in bytes.
const carryTail = 16 << 10

// carryGrace is how long a cancelled child has after SIGTERM before its process tree is killed.
const carryGrace = 5 * time.Second

// noRecordFmt is the outer error when the carried ask left no finished record; it takes the exit code.
const noRecordFmt = "carried ask exited %d without finishing a dispatch record"

// takeoverBrief is the fixed Pi takeover brief: agent, cutoff, the original task, the partial output.
const takeoverBrief = `A carried %s run was cut off (%s) before it finished.
Continue the task below from where it stopped and finish it; say what you changed.
Read every file named below that you need, and batch all of those independent reads into one response.
Keep it small: read only what the task or the partial output names, never survey the repository to start over.

Task:
%s

Partial output (last %d bytes):
%s
`

// carryExec runs one child ask line of a carrier and returns its exit code; tests swap it.
var carryExec = func(ctx context.Context, argv []string, stdin io.Reader, env []string, stdout, stderr io.Writer) int {
	exe, err := os.Executable()
	if err == nil && proc.IsSelfUnderTest(exe) {
		err = errors.New("refusing to relaunch the test binary")
	}
	if err != nil {
		fmt.Fprintf(stderr, "ask: carry: %v\n", err)
		return 1
	}
	cmd := exec.CommandContext(ctx, exe, argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr, cmd.Env = stdin, stdout, stderr, env
	proc.TreeCancel(cmd, carryGrace) // own group; SIGTERM lets the child finalize its record, then the tree is killed
	cmd.WaitDelay = 2 * carryGrace   // past TreeCancel's own kill: only a pipe a detached grandchild holds is left
	err = cmd.Run()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return exit.ExitCode()
	}
	if err != nil {
		fmt.Fprintf(stderr, "ask: carry: %v\n", err)
		return 1
	}
	return 0
}

// carryMode runs a --carry ask, or the --job child of a carried --background, to the end; other runs pass.
func (r *askRun) carryMode() (int, bool) {
	f := r.f
	if f.job != "" {
		return r.carryJob()
	}
	if !f.carry {
		return 0, false
	}
	if err := carryCheck(r); err != nil {
		return r.fail(err, 2)
	}
	prompt, err := resolveAskPrompt(strings.Join(r.pos, " ")) // stdin passes through byte-identical, capped
	if err != nil {
		return r.fail(err, 1)
	}
	if strings.TrimSpace(prompt) == "" {
		return r.fail(errors.New("no task: give a prompt, or \"-\" to read stdin"), 2)
	}
	rec, err := r.carryRecord(prompt)
	if err == nil {
		err = takeClaim(nil, rec.ID)
	}
	if _, usage := errors.AsType[*agent.UsageError](err); usage {
		return r.fail(err, 2)
	}
	if err != nil {
		return r.fail(err, 1)
	}
	if f.background {
		return carrySpawn(r.ctx, rec, f.format), true
	}
	return runCarry(r.ctx, rec, r.out), true
}

// carryJob runs a carried --background record in this --job child; any other --job record passes on.
func (r *askRun) carryJob() (int, bool) {
	rec, err := agent.LoadRecord(r.f.job)
	if err != nil || !rec.Request.Carry {
		return 0, false // the --job path reports a bad id
	}
	if rec, err = agent.ClaimJob(rec.ID); err != nil {
		return r.fail(err, 2)
	}
	rec.PIDStart, _ = proc.StartTime(rec.PID) // ClaimJob clears it; the carried child's ancestor check needs it
	if err := agent.NotePID(rec, true); err != nil {
		fmt.Fprintf(os.Stderr, "ask: carry: note pid of %s: %v\n", rec.ID, err)
	}
	return runCarry(r.ctx, rec, r.out), true
}

// carryCheck refuses the run shapes --carry cannot pass through, then an agent whose row does not carry.
func carryCheck(r *askRun) error {
	f := r.f
	switch {
	case f.live || f.batchSrc != "" || f.loop || f.loopResume != "" || f.waves != "" || cmp.Or(f.retry, f.host) != "" || f.dryRun:
		return errors.New("--carry runs one plain task: not with --live, --batch, --loop, --waves, --retry, --host or --dry-run")
	case len(f.env) > 0:
		return errors.New("--carry cannot pass --env: its values would ride the child's command line")
	case f.overlay == "-":
		return errors.New("--carry gives the task on stdin: pass --overlay as a file or inline JSON")
	}
	return carryAgent(f.name, r.persona != nil)
}

// carryAgent refuses pi, claude, a persona and any agent whose roster row lacks Carry.
func carryAgent(name string, persona bool) error {
	if name == agentid.Pi || name == agentid.Claude || persona {
		return fmt.Errorf("--carry runs an external agent directly; %s is not carried", name)
	}
	if s, ok := agent.LookupSchema(name); !ok || !s.CLI.Carry {
		return fmt.Errorf("--carry: the %s row does not set Carry: true (agent.CLI.Carry)", name)
	}
	return nil
}

// carryRecord claims and saves the outer record: request.carry, the inner line (task on stdin), the permissions.
func (r *askRun) carryRecord(prompt string) (agent.Record, error) {
	f := r.f
	if err := r.fs.Set("agent", f.name); err != nil { // the inner line names the agent even under an `<agent> ask` verb
		return agent.Record{}, err
	}
	folded := foldedArgv(r.fs, r.ov)
	argv := canonicalArgv(r.fs, f.listValues(), prompt, true, nil, "carry", "background")
	_, values := agent.RunValues(r.ctx, nil)
	typed := f.modelTyped || slices.Contains(folded, "--model") && !agent.IsAuto(f.model) // the child types it
	req := agent.RunOptions{Prompt: prompt, Agent: f.name, Model: f.model, ModelTyped: typed, Tools: f.tools,
		ReadOnly: f.readOnly, WorkDir: f.workdir, Timeout: f.timeout, TimeoutTyped: f.timeoutTyped, Permissions: r.perms,
		Carry: true, CarryArgv: slices.Insert(argv, len(argv)-1, folded...), Values: values}
	rec, err := agent.ClaimAskRecord(r.ctx, f.name, req, f.workdir, f.background)
	if err != nil {
		return rec, err
	}
	rec.FallbackPolicy, rec.Background = f.fallback, f.background // background: the --job child claims the pid
	if !f.background {
		rec.PIDStart, _ = proc.StartTime(rec.PID) // the carried child's live ancestor
	}
	return rec, rec.Save()
}

// foldedArgv is the model, effort and fallback the overlay folded in untyped, as typed flags for the inner line.
func foldedArgv(fs *flag.FlagSet, ov agentconfig.Overlay) []string {
	var out []string
	for _, kv := range [][2]string{{"model", ov.Model}, {"effort", ov.Effort}, {"fallback", ov.Fallback}} {
		if kv[1] != "" && !helptext.Explicit(fs, kv[0]) {
			out = append(out, "--"+kv[0], fs.Lookup(kv[0]).Value.String())
		}
	}
	return out
}

// carrySpawn detaches the carrier as an `ask --job` child that runs runCarry; follow it with `agents result -f`.
func carrySpawn(ctx context.Context, rec agent.Record, format string) int {
	if err := agent.SpawnArgvContext(ctx, &rec, []string{"agents", "ask", "--job", rec.ID}, nil); err != nil {
		return failAsk(format, rec.Agent, failSpawn(&rec, err))
	}
	if err := reportBackgroundStart(format, rec); err != nil {
		return helptext.Fail("%s: job %s started; write job id: %v", rec.Agent, rec.ID, err)
	}
	return 0
}

// runCarry runs rec's inner line, then a Pi takeover when the child was cut off; it finishes rec and returns the exit.
func runCarry(ctx context.Context, rec agent.Record, out io.Writer) int {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if rec.Background {
		admitCarry(ctx, &rec)
		if err := ctx.Err(); err != nil {
			agent.RecordRefusal(rec.Agent, rec.Request, &rec, err)
			return 1
		}
	}
	out, errOut, log, closeLog := carryLog(rec, out)
	out, log, endStream := streamCarry(rec, out, log)
	start, tail := time.Now(), &tailWriter{max: carryTail}
	stopForward := agent.StartForward(ctx, rec.ID, "", log)
	code := carryExec(ctx, rec.Request.CarryArgv, strings.NewReader(rec.Request.Prompt), carryEnv(ctx, rec), io.MultiWriter(out, tail), errOut)
	stopForward()
	child, found := agent.LatestChild(rec.ID)
	if found {
		child = settleChild(child, code)
	}
	final := child
	take, why := takeoverDecision(child, found, rec.FallbackPolicy, rec.Request.ModelTyped)
	if why != "" {
		fmt.Fprintln(os.Stderr, "ask: carry:", why)
	}
	if take && ctx.Err() == nil {
		if pcode, pi, ok := runTakeover(ctx, rec, child, string(tail.b), out, log); ok {
			code, final = pcode, pi
		}
	}
	closeLog()
	env := finishCarry(rec, final, found, code, time.Since(start))
	endStream(env)
	return code
}

// streamCarry gives a foreground stream-json carrier via-stamped child lines, a takeover event and its one result.
func streamCarry(rec agent.Record, raw io.Writer, log wire.StreamLog) (io.Writer, wire.StreamLog, func(wire.Envelope)) {
	fs, f := askFlagSet()
	fs.SetOutput(io.Discard)
	argv := rec.Request.CarryArgv
	if rec.Background || len(argv) < 3 {
		return raw, log, func(wire.Envelope) {}
	}
	if _, err := helptext.ParseArgs(fs, argv[3:]); err != nil || f.format != agent.FormatStreamJSON {
		return raw, log, func(wire.Envelope) {}
	}
	put := func(ev wire.Event) {
		ev.Schema = wire.EventSchema
		_ = json.NewEncoder(raw).Encode(ev)
	}
	out := &viaLines{w: raw, carrier: rec.ID}
	return out, func(kind, text string) {
		log.Emit(kind, text)
		if kind == wire.EventTakeover {
			put(wire.Event{DispatchID: rec.ID, Type: kind, Text: text})
		}
	}, func(env wire.Envelope) { put(wire.ResultEvent(env)) } // the child's own result lines were dropped
}

// viaLines feeds viaWriter whole lines, each stamped with its own dispatch id; the carrier's own lines get no via.
type viaLines struct {
	w       io.Writer
	carrier string
	buf     []byte
}

func (v *viaLines) Write(p []byte) (int, error) {
	v.buf = append(v.buf, p...)
	for {
		i := bytes.IndexByte(v.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		var id struct {
			DispatchID string `json:"dispatch_id"`
		}
		_ = json.Unmarshal(v.buf[:i], &id)
		if id.DispatchID == v.carrier {
			id.DispatchID = ""
		}
		_, err := viaWriter{w: v.w, child: id.DispatchID}.Write(v.buf[:i+1])
		v.buf = v.buf[i+1:]
		if err != nil {
			return 0, err
		}
	}
}

// admitCarry publishes the dry-run route before the daemon reads the carrier's pool.
func admitCarry(ctx context.Context, rec *agent.Record) {
	routed, err := carryRoute(ctx, *rec)
	if err == nil {
		rec.Route = routed.Overlay().Route()
		_, _, err = agent.UpdateRunning(rec.ID, func(r agent.Record) agent.Record {
			r.Route = rec.Route
			return r
		})
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ask: carry: admission route: %v; running directly\n", err)
		return
	}
	admitPending(ctx, rec)
}

// carryRoute peeks through the inner ask's flag/overlay phases and the existing dry-run resolver, without Prepare.
func carryRoute(ctx context.Context, rec agent.Record) (agent.ModelRoute, error) {
	if len(rec.Request.CarryArgv) < 3 {
		return agent.ModelRoute{}, errors.New("no inner line")
	}
	fs, f, args, pos, code, done := parseAsk("", rec.Request.CarryArgv[3:])
	if done {
		return agent.ModelRoute{}, fmt.Errorf("parse inner ask: exit %d", code)
	}
	if !helptext.Explicit(fs, "fallback") {
		f.fallback = router.DefaultFallback()
	}
	f.dryRun = true
	r := &askRun{ctx: agent.PeekContext(ctx), f: f, fs: fs, args: args, pos: pos, out: io.Discard}
	markTyped(fs, f)
	for _, phase := range []func() (int, bool){r.config, r.prelude, r.selectAgent, r.resolveRecord,
		r.checkArm, r.resolveLead, r.parsePerms, r.export} {
		if code, done := phase(); done {
			return agent.ModelRoute{}, fmt.Errorf("resolve inner ask: exit %d", code)
		}
	}
	req := r.request(rec.Request.Prompt)
	if run := r.armRun(); run != nil {
		req = run.probe(req)
	}
	ov, err := resolveOverlay(r.ov, f.env, f.keepEnv, false)
	if err != nil {
		return agent.ModelRoute{}, err
	}
	req = req.ApplyOverlay(r.ctx, ov)
	ctx = providers.WithIdentity(r.ctx, cmp.Or(f.as, providers.IdentityHeadless))
	o, _, _, err := newClient().Plan(ctx, req)
	if err != nil {
		return agent.ModelRoute{}, err
	}
	route, _, err := dryRoute(ctx, o, req)
	return route, err
}

// carryLog is the answer, trailer and event-line writers into rec's log; a background carrier shares one stream.
func carryLog(rec agent.Record, out io.Writer) (io.Writer, io.Writer, wire.StreamLog, func()) {
	if rec.Background {
		locked := &lockedWriter{w: out} // the child's stdout and stderr copy from two goroutines
		return locked, locked, locked.event, func() {}
	}
	sink := wire.NewLogSink(rec.LogPath())
	log := wire.EventLogFor(wire.WithLog(context.Background(), sink))
	return io.MultiWriter(out, sink.Stream()), os.Stderr, log, sink.Close
}

// lockedWriter serializes writes to w, which is not safe for concurrent use.
type lockedWriter struct {
	mu      sync.Mutex
	w       io.Writer
	partial bool
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, err := l.w.Write(p)
	if n > 0 {
		l.partial = p[n-1] != '\n'
	}
	return n, err
}

func (l *lockedWriter) event(kind, text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	prefix := ""
	if l.partial {
		prefix = "\n"
	}
	line := []byte(prefix + "▸ " + kind + ": " + text + "\n")
	n, err := l.w.Write(line)
	if n > 0 {
		l.partial = line[n-1] != '\n'
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ask: carry: log event: %v\n", err)
	}
}

// settleChild is child once its process has exited: a record left running is ended, since nothing else will end it.
func settleChild(child agent.Record, code int) agent.Record {
	if child.Status != "running" {
		return child
	}
	fixed, ok, err := agent.RepairDead(child.ID, func(r agent.Record) agent.Record {
		r.Status, r.FailKind, r.Error = "error", agent.FailAvailability, fmt.Sprintf(noRecordFmt, code)
		return r
	})
	if err == nil && ok {
		return fixed
	}
	if again, lerr := agent.LoadRecord(child.ID); lerr == nil { // it finished between the reads
		return again
	}
	return child
}

// takeoverDecision: a cut-off child goes to Pi, unless --fallback none pins a typed model (refused, with why).
func takeoverDecision(child agent.Record, found bool, policy string, modelTyped bool) (take bool, why string) {
	if !found || !child.CutOff() && child.Status != wire.StatusPartial && child.Status != wire.StatusStalled {
		return false, ""
	}
	if policy == router.FallbackNone && modelTyped {
		return false, fmt.Sprintf("%s was cut off (%s); no Pi takeover: --fallback none with a typed model", child.ID, cmp.Or(child.Cutoff, child.Status))
	}
	return true, ""
}

// runTakeover starts Pi under the inner line's tools and permissions on the fixed brief; false: Pi left no record.
func runTakeover(ctx context.Context, rec, child agent.Record, partial string, out io.Writer, log wire.StreamLog) (int, agent.Record, bool) {
	argv, err := takeoverArgv(ctx, rec.Request.CarryArgv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ask: carry: no Pi takeover: %v\n", err)
		return 0, agent.Record{}, false
	}
	cut := cmp.Or(child.Cutoff, child.Status)
	fmt.Fprintf(os.Stderr, "ask: carry: %s was cut off (%s); pi takes over\n", child.ID, cut)
	log.Emit(wire.EventTakeover, fmt.Sprintf("%s cut off (%s); pi takes over", child.ID, cut))
	brief := fmt.Sprintf(takeoverBrief, rec.Agent, cut, rec.Request.Prompt, len(partial), partial)
	stopForward := agent.StartForward(ctx, rec.ID, child.ID, log)
	code := carryExec(ctx, argv, strings.NewReader(brief), carryEnv(ctx, rec), out, carryErr(rec, out))
	stopForward()
	pi, ok := agent.LatestChild(rec.ID)
	if !ok || pi.ID == child.ID || pi.Agent != agentid.Pi {
		fmt.Fprintf(os.Stderr, "ask: carry: pi takeover left no record (exit %d)\n", code)
		return code, agent.Record{}, false
	}
	pi = settleChild(pi, code)
	pi.TakeoverOf = child.ID
	if err := agent.MarkTakeover(pi.ID, child.ID); err != nil {
		fmt.Fprintf(os.Stderr, "ask: carry: link takeover %s: %v\n", pi.ID, err)
	}
	if pi.Status == "ok" {
		fmt.Fprintf(os.Stderr, "ask: finished by pi (%s)\n", cmp.Or(pi.Model, pi.Invoked, "default"))
	} else {
		fmt.Fprintf(os.Stderr, "ask: pi takeover %s ended %s\n", pi.ID, pi.Status)
	}
	return code, pi, true
}

// carryErr is a takeover's stderr: the foreground trailer stays on os.Stderr, a background one joins the answer stream.
func carryErr(rec agent.Record, out io.Writer) io.Writer {
	if rec.Background {
		return out
	}
	return os.Stderr
}

// takeoverArgv is Pi's line: the inner line's tools tier, permission, workdir, format and overlay-cap flags.
func takeoverArgv(ctx context.Context, inner []string) ([]string, error) {
	fs, f := askFlagSet()
	fs.SetOutput(io.Discard)
	if len(inner) < 3 {
		return nil, errors.New("no inner line")
	}
	if _, err := helptext.ParseArgs(fs, inner[3:]); err != nil {
		return nil, err
	}
	if len(f.permAsk) > 0 {
		return nil, errors.New("pi refuses --ask rules (its guard only allows or blocks)")
	}
	argv := []string{"claude-code", "agents", "ask", "--agent", agentid.Pi, "--format", f.format}
	if f.tools { // a toolless carried run gets a toolless Pi
		argv = append(argv, "--tools")
	}
	if f.readOnly {
		argv = append(argv, "--read-only")
	}
	for _, kv := range [][2]string{{"permissions", f.permMode}, {"workdir", f.workdir}} {
		if kv[1] != "" {
			argv = append(argv, "--"+kv[0], kv[1])
		}
	}
	for _, rule := range f.permAllow {
		argv = append(argv, "--allow", rule)
	}
	for _, rule := range f.permDeny {
		argv = append(argv, "--deny", rule)
	}
	caps, err := overlayCaps(ctx, f.overlay)
	return append(append(argv, caps...), "-"), err
}

// overlayCaps is --overlay with only value's caps: the inner route is the carried agent's, not Pi's.
func overlayCaps(ctx context.Context, value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	o, err := agentconfig.LoadOverlayContext(ctx, value, nil)
	if err != nil {
		return nil, err
	}
	if caps := agentconfig.InlineOverlay("", "", o); caps != "" {
		return []string{"--overlay", caps}, nil
	}
	return nil, nil
}

// carryEnv is a child's env: the run values with rec as its parent dispatch.
func carryEnv(ctx context.Context, rec agent.Record) []string {
	_, values := agent.RunValues(ctx, nil)
	values = values.Merge(proc.Values{proc.DispatchIDEnv: rec.ID, proc.DispatchDepthEnv: strconv.Itoa(rec.Depth)})
	return agent.EnvSpec{Mode: agent.EnvClaude}.Environ(proc.WithValues(ctx, values))
}

// finishCarry gives rec the final child's result event, answer and outcome; children carry the usage, so it holds none.
func finishCarry(rec, final agent.Record, found bool, code int, took time.Duration) wire.Envelope {
	env := carryEnvelope(rec, final, found, code, took)
	events := wire.NewEventLog(rec.EventsPath(), rec.ID)
	err := events.Emit(wire.ResultEvent(env)) // before the record leaves running, so a follower never misses it
	events.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ask: carry: result of %s: %v\n", rec.ID, err)
	}
	_, _, err = agent.UpdateRunning(rec.ID, func(r agent.Record) agent.Record {
		r.DurationMs = took.Milliseconds()
		if !found || final.Status == "running" {
			r.Status, r.FailKind, r.Error = "error", agent.FailAvailability, fmt.Sprintf(noRecordFmt, code)
			return r
		}
		r.Status, r.Error, r.FailKind, r.Cutoff = final.Status, final.Error, final.FailKind, final.Cutoff
		r.Provider, r.Model, r.Invoked, r.RanRoute = final.Provider, final.Model, final.Invoked, final.RanRoute
		if who := cmp.Or(final.RanAs, final.Agent); who != r.Agent { // a child's fallback, or the Pi takeover, answered
			r.Fallback, r.RanAs = r.Agent, who
			r.FallbackReason = cmp.Or(final.FallbackReason, "pi takeover of "+final.TakeoverOf)
		}
		return r
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ask: carry: finish %s: %v\n", rec.ID, err)
	}
	return env
}

// carryEnvelope is the final child's result envelope under rec's id, or an error one when no child finished.
func carryEnvelope(rec, final agent.Record, found bool, code int, took time.Duration) wire.Envelope {
	if !found || final.Status == "running" {
		res := wire.Result{Agent: rec.Agent, DurationMs: took.Milliseconds()}
		return wire.NewEnvelope(res, fmt.Errorf(noRecordFmt, code), rec.ID)
	}
	env := final.ResultEnvelope(io.Discard)
	env.DispatchID, env.DurationMs = rec.ID, took.Milliseconds()
	if env.Agent != rec.Agent && env.Fallback == nil {
		env.Fallback, env.FallbackReason = &rec.Agent, cmp.Or(env.FallbackReason, "pi takeover of "+final.TakeoverOf)
	}
	return env
}

// tailWriter keeps the last max bytes written.
type tailWriter struct {
	max int
	b   []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if over := len(t.b) - t.max; over > 0 {
		t.b = slices.Clone(t.b[over:])
	}
	return len(p), nil
}
