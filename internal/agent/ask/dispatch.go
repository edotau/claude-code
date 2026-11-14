package ask

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/roles"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
	usagepkg "github.com/edotau/claude-code/internal/usage"
)

// DispatchOptions are `agents dispatch`'s run flags: the role, --output-schema, --jobs, --format and --background.
type DispatchOptions struct {
	Role, Schema, Format string
	Jobs                 int
	Background           bool
	Route                *RouteFlags // the flags shared with ask; nil applies none
}

// RouteFlags holds the ask flags dispatch shares: --overlay, --fallback and the permission flags.
type RouteFlags struct{ f askFlags }

// unsupportedKey names an overlay key dispatch cannot honour per leg ("" when none is set).
func unsupportedKey(ov agentconfig.Overlay) string {
	switch {
	case ov.Model != "":
		return "model"
	case ov.Effort != "":
		return "effort"
	case ov.Host != "":
		return "host"
	}
	return ""
}

// RegisterRouteFlags adds those flags to fs through ask's own definitions.
func RegisterRouteFlags(fs *flag.FlagSet) *RouteFlags {
	r := &RouteFlags{}
	registerFallbackFlag(fs, &r.f)
	registerPermFlags(fs, &r.f)
	registerOverlayFlag(fs, &r.f)
	return r
}

// Args re-emits the route flags that were set as `ask` arguments, so a child ask run routes as the caller did.
func (r *RouteFlags) Args() ([]string, error) {
	f := &r.f
	if f.overlay == "-" {
		return nil, errors.New("--overlay - reads stdin, which a batch child needs; pass a file or inline JSON")
	}
	var out []string
	for _, kv := range [][2]string{{"overlay", f.overlay}, {"fallback", f.fallback}, {"mode", f.permMode}} {
		if kv[1] != "" {
			out = append(out, "--"+kv[0], kv[1])
		}
	}
	for _, list := range []struct {
		name  string
		rules []string
	}{{"allow", f.permAllow}, {"ask", f.permAsk}, {"deny", f.permDeny}} {
		for _, rule := range list.rules {
			out = append(out, "--"+list.name, rule)
		}
	}
	return out, nil
}

// apply resolves the flags and overlay as ask does onto q; the returned ctx holds the confinement, egress and overlay.
func (r *RouteFlags) apply(ctx context.Context, q *roles.Request, agentName, format string, background bool) (context.Context, error) {
	f := &r.f
	perms, err := agent.ParsePermissions(f.permMode, f.permAllow, f.permAsk, f.permDeny)
	if err != nil {
		return ctx, err
	}
	ov, err := LoadOverlay(ctx, f.overlay, "")
	if err != nil {
		return ctx, err
	}
	if key := unsupportedKey(ov); key != "" {
		return ctx, fmt.Errorf("overlay key %q is not supported by dispatch (use ask)", key)
	}
	fallback := cmp.Or(f.fallback, ov.Fallback)
	if err := validateAskFlags(format, fallback); err != nil {
		return ctx, err
	}
	resolved, err := resolveOverlay(ov, nil, nil, background)
	if err != nil {
		return ctx, err
	}
	if ctx, err = r.confine(ctx, q, ov, agentName, fallback); err != nil {
		return ctx, err
	}
	if ctx, err = overlayEgress(ctx, ov.Remote); err != nil {
		return ctx, err
	}
	if ctx, err = carryOverlay(ctx, ov); err != nil {
		return ctx, err
	}
	q.Permissions, q.Fallback, q.Overlay = perms, fallback, resolved
	return ctx, nil
}

// confine applies the overlay's confinement and puts the auto pick and the narrowed lead on q, as ask's checkArm does.
func (r *RouteFlags) confine(ctx context.Context, q *roles.Request, ov agentconfig.Overlay, agentName,
	fallback string) (context.Context, error) {
	f := &r.f
	picks := armAutoPick(ctx, ov.Confinement())
	ctx, err := ApplyCoworker(ctx, ov.Confinement(), "", agentName, "", fallback)
	if err != nil {
		return ctx, err
	}
	if picks {
		q.Auto = router.ArmProfileContext(ctx)
	}
	typed := f.fallback != ""
	f.fallback = fallback
	lead, err := narrowLead(agentName, f, confineLead(ctx, agentName, f) || typed)
	q.Lead, q.Profile = lead, f.provider
	return ctx, err
}

// Dispatch is `agents dispatch`: resolve the role and --output-schema, then run or detach the legs.
func Dispatch(q roles.Request, o DispatchOptions) int {
	if len(q.Parts) > roles.MaxParts {
		return helptext.FailUsage("dispatch: %d --part values; the fan-out cap is %d", len(q.Parts), roles.MaxParts)
	}
	role, err := roles.Lookup(o.Role)
	if err != nil {
		return helptext.FailUsage("dispatch: %v", err)
	}
	if q.OutputSchema, err = resolveOutputSchema(o.Schema); err != nil {
		return helptext.FailUsage("dispatch: %v", err)
	}
	ctx := context.Background()
	if o.Route != nil {
		if ctx, err = o.Route.apply(ctx, &q, role.Agent, o.Format, o.Background); err != nil {
			return helptext.FailUsage("dispatch: %v", err)
		}
	}
	_, q.Values = agent.RunValues(ctx, nil)
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return dispatchRun(ctx, role, q, o.Jobs, o.Format, o.Background)
}

// dispatchRun builds the legs (splitting explore when no --part) and runs or detaches them.
func dispatchRun(ctx context.Context, role roles.Role, q roles.Request, jobs int, format string, background bool) int {
	cl := newClient()
	if q.Values != nil {
		ctx = proc.WithValues(ctx, q.Values)
	}
	if role.Name == "explore" && len(q.Parts) == 0 && !background {
		q.Parts = roles.Split(ctx, cl, q.Task)
	}
	legs := roles.Legs(ctx, role, q)
	if len(legs) == 0 {
		return helptext.FailUsage("dispatch: role %q builds no legs (a renamed role must be explore, fleet or compare)", role.Name)
	}
	if background {
		return dispatchDetach(ctx, cl, legs, format)
	}
	var progress io.Writer = os.Stderr
	if format == helptext.JSON {
		progress = nil
	}
	rep := roles.RunWith(ctx, role, legs, max(1, jobs), progress, legStream(cl, q.Auto, q.Fallback))
	if err := writeReport(rep, format); err != nil {
		return helptext.Fail("dispatch: write report: %v", err)
	}
	if rep.Status == "error" {
		return 1
	}
	return 0
}

// writeReport prints rep to stdout as one JSON line or as text, returning any write failure.
func writeReport(rep roles.Report, format string) error {
	if format == helptext.JSON {
		return agent.WriteJSONLine(os.Stdout, rep)
	}
	return rep.Text(os.Stdout)
}

// dispatchDetach spawns each leg as `ask --background` does and prints its id (one JSON line per leg with json).
func dispatchDetach(ctx context.Context, client *agent.Client, legs []roles.Leg, format string) int {
	code := 0
	for _, leg := range legs {
		if leg.Err != "" {
			code = helptext.Fail("dispatch: %s: %s", leg.Label, leg.Err)
			continue
		}
		rec, err := spawnLeg(ctx, client, leg)
		if err != nil {
			code = helptext.Fail("dispatch: %s: %v", leg.Label, err)
			continue
		}
		if err := printLegID(leg.Label, rec, format); err != nil {
			code = helptext.Fail("dispatch: %s: write job id: %v", leg.Label, err)
		}
	}
	return code
}

// spawnLeg saves and detaches one leg; a failed spawn finishes its record so it never shows as running.
func spawnLeg(ctx context.Context, client *agent.Client, leg roles.Leg) (agent.Record, error) {
	if err := agent.CheckCapabilities(leg.Opts.Agent, leg.Opts); err != nil {
		return agent.Record{}, err
	}
	leg.Opts.Background = true // the saved record must not carry this short-lived process's pid
	prepared, err := client.Prepare(ctx, leg.Opts, nil)
	if err != nil {
		return agent.Record{}, err
	}
	if err := prepared.Record.Save(); err != nil {
		return agent.Record{}, err
	}
	if prepared.Opts.Values != nil {
		ctx = proc.WithValues(ctx, prepared.Opts.Values)
	}
	if err := agent.SpawnContext(ctx, &prepared.Record, nil); err != nil {
		return agent.Record{}, failSpawn(&prepared.Record, err)
	}
	return prepared.Record, nil
}

// failSpawn finishes a record whose spawn failed so it never shows as running; a failed save joins the returned error.
func failSpawn(rec *agent.Record, err error) error {
	rec.Finish(wire.Result{}, err, time.Since(rec.Time))
	if saveErr := rec.Save(); saveErr != nil {
		err = errors.Join(err, fmt.Errorf("save failed dispatch %s: %w", rec.ID, saveErr))
	}
	return err
}

// printLegID prints a detached leg's id: one JSON line, or the label and ask's background-start text.
func printLegID(label string, rec agent.Record, format string) error {
	if format == helptext.JSON {
		return agent.WriteJSONLine(os.Stdout, struct {
			Label string `json:"label"`
			ID    string `json:"id"`
			Log   string `json:"log"`
		}{label, rec.ID, rec.Log})
	}
	if _, err := fmt.Printf("%s\t", label); err != nil {
		return err
	}
	return reportBackgroundStart(helptext.Text, rec)
}

// legStream is Client.Stream for a dispatch leg; with an auto-picked workspace a 429 moves to the next one, as in ask.
func legStream(cl *agent.Client, auto, fallback string) roles.Runner {
	return func(ctx context.Context, o agent.RunOptions) <-chan wire.Event {
		if err := agent.CheckCapabilities(o.Agent, o); err != nil {
			ch := make(chan wire.Event, 1)
			ch <- wire.ResultEvent(wire.FailureEnvelope(o.Agent, err))
			close(ch)
			return ch
		}
		if auto == "" {
			return cl.Stream(ctx, o)
		}
		ch := make(chan wire.Event, 64)
		go func() {
			defer close(ch)
			a := &armRetry{agentName: cmp.Or(o.Agent, "claude"), model: o.Model, fallback: fallback, tried: []string{auto}}
			ch <- wire.ResultEvent(a.streamLeg(ctx, cl, o, ch))
		}()
		return ch
	}
}

// streamLeg runs o through the workspace retry, sending its events (not results) to ch; the final envelope.
func (a *armRetry) streamLeg(ctx context.Context, cl *agent.Client, o agent.RunOptions, ch chan<- wire.Event) (env wire.Envelope) {
	defer func() {
		if r := recover(); r != nil {
			env = wire.FailureEnvelope(a.agentName, fmt.Errorf("panic: %v", r))
		}
	}()
	o.AutoArm = true
	req := a.probe(o)
	leg := *cl // startPrepared and the retry rewrite Notify: each leg owns its copy of the shared client
	cl = &leg
	p, err := a.prepare(ctx, cl, req, nil)
	if err != nil {
		return wire.FailureEnvelope(a.agentName, err)
	}
	_ = startPrepared(cl, p, false)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	st := wire.NewStamper()
	sink := sinkFunc(func(ev wire.Event) error {
		if ev.Type == wire.EventResult || ev.Type == wire.EventProgress {
			return nil
		}
		st.Stamp(&ev)
		return sendLegEvent(ctx, cancel, ch, ev)
	})
	p, res, err := a.attempts(ctx, cl, p, req, func(ctx context.Context, p *agent.Prepared) (wire.Result, error) {
		return cl.Exec(ctx, p, sink, agent.RunRecordedOpts{})
	})
	if pf, ok := errors.AsType[*prepareFailure](err); ok {
		return wire.FailureEnvelope(a.agentName, pf.err)
	}
	return wire.NewEnvelope(res, err, p.Record.ID)
}

// armStreamStall is how long a leg waits to hand an event to a consumer that stopped draining before it gives up.
var armStreamStall = 2 * time.Minute

// errLegAbandoned is the cancellation cause when a leg's consumer stops draining.
var errLegAbandoned = errors.New("dispatch: stream consumer abandoned")

// sendLegEvent hands ev to ch; a cancelled ctx or a consumer stalled past armStreamStall ends the leg.
func sendLegEvent(ctx context.Context, cancel context.CancelCauseFunc, ch chan<- wire.Event, ev wire.Event) error {
	t := time.NewTimer(armStreamStall)
	defer t.Stop()
	select {
	case ch <- ev:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-t.C:
		cancel(errLegAbandoned)
		return errLegAbandoned
	}
}

// sinkFunc adapts a function to wire.Sink.
type sinkFunc func(wire.Event) error

func (f sinkFunc) Emit(ev wire.Event) error { return f(ev) }

// RouteRefresh re-fetches a stale account for a pick; a var so tests stay offline.
var RouteRefresh = usagepkg.RefreshAccount

// newClient builds the dispatch client; RouteRefresh is the offline-able account refresh.
func newClient() *agent.Client {
	return agent.NewClient(router.RuleClassifier{}.Alias, RouteRefresh,
		func(s string) { fmt.Fprintln(os.Stderr, "ask:", s) })
}
