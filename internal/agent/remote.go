package agent

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
)

// remoteGrace pads --timeout for the local ssh's silence watchdog (reset per remote line); the remote's verdict wins.
var remoteGrace = 30 * time.Second

// idleTimer is the slice of *time.Timer the silence watchdog uses.
type idleTimer interface {
	Reset(time.Duration) bool
	Stop() bool
}

// newIdleTimer is the watchdog seam; tests swap in a timer they drive by hand.
var newIdleTimer = func(d time.Duration, f func()) idleTimer { return time.AfterFunc(d, f) }

// remoteEngine runs the named agent through `claude-code agents ask` on another host over ssh; the remote applies the caps.
type remoteEngine struct{ agent, host, bin, fallback string }

// newRemoteEngine wraps agentName for host; bin is the remote claude-code path (see remote.Bin), fallback the remote's policy.
func newRemoteEngine(agentName, host, bin, fallback string) Engine {
	return remoteEngine{agentName, host, bin, fallback}
}

func (r remoteEngine) Name() string { return r.agent }

// Caps is Persona-native only: the remote's applyCaps does the dropping, and Exec calls Run directly.
func (r remoteEngine) Caps() Caps { return Caps{Persona: Native} }

func (r remoteEngine) Available() error { return nil }

// remoteDropped names the flags that mean nothing on another host.
func remoteDropped(req RunOptions) []string {
	var out []string
	if len(req.Attach) > 0 {
		out = append(out, "--attach")
	}
	if len(req.KeepEnv) > 0 {
		out = append(out, "--keep-env")
	}
	if req.DebugFile != "" {
		out = append(out, "--debug-file="+req.DebugFile)
	}
	return out
}

// remoteTarget is the --agent value the remote side resolves.
func remoteTarget(agentName string, req RunOptions) string {
	if req.Persona != nil {
		return agentName + ":" + req.Persona.Name
	}
	return agentName
}

// remoteScript is the POSIX sh line run on the remote: every element is single-quoted, the prompt is never in it.
// --env values do ride this line, so they are visible to the remote `ps` (and, base64-wrapped, to the local one).
func remoteScript(bin, agentName, fallback string, req RunOptions) (string, error) {
	if err := validateRemoteScript(bin, agentName, req); err != nil {
		return "", err
	}
	execBin, err := remoteExec(bin)
	if err != nil {
		return "", fmt.Errorf("remote ask: %w", err)
	}
	w := []string{execBin, "agents", "ask", "--format", "stream-json", "--fallback", textutil.ShellQuote(cmp.Or(fallback, router.FallbackNone)),
		"--stdin-framed", "--agent", textutil.ShellQuote(remoteTarget(agentName, req))}
	opt := func(flag, v string) {
		if v != "" {
			w = append(w, flag, textutil.ShellQuote(v))
		}
	}
	opt("--model", req.Model)
	opt("--profile", req.Provider)
	opt("--effort", req.Effort)
	opt("--as", req.Identity)
	opt("--overlay", remoteOverlay()) // ApplyCoworker exports the confinement as env, which ssh does not carry
	opt("--session", req.SessionID)
	opt("--system-prompt", req.SystemPrompt)
	opt("--append-system-prompt", req.AppendSystemPrompt)
	opt("--output-schema", req.OutputSchema)
	if req.WorkDir != "" {
		w = append(w, "--workdir", textutil.QuotePath(remote.HomePath(req.WorkDir)))
	}
	if req.Timeout > 0 {
		opt("--timeout", req.Timeout.String())
	}
	for _, kv := range req.Env {
		opt("--env", kv)
	}
	opt("--mode", req.Permissions.Mode)
	for _, set := range []struct {
		flag  string
		rules []Rule
	}{{"--allow", req.Permissions.Allow}, {"--ask", req.Permissions.Ask}, {"--deny", req.Permissions.Deny}} {
		for _, s := range RuleStrings(set.rules) {
			opt(set.flag, s)
		}
	}
	for _, b := range []struct {
		on   bool
		flag string
	}{{req.Tools, "--tools"}, {req.ReadOnly, "--read-only"}, {req.NoTools, "--no-tools"}, {req.Autonomous, "--autonomous"}, {req.Fleet, "--fleet"}, {req.EnableMemory, "--enable-memory"},
		{req.NoPersist, "--no-persist"}, {req.SafeMode, "--safe-mode"}, {req.Direct, "--direct"}} {
		if b.on {
			w = append(w, b.flag)
		}
	}
	return strings.Join(w, " "), nil
}

func validateRemoteScript(bin, agentName string, req RunOptions) error {
	vals := []string{bin, agentName, req.Model, req.Provider, req.Effort, req.Identity, remoteOverlay(), req.WorkDir, req.SessionID, req.SystemPrompt,
		req.AppendSystemPrompt, req.OutputSchema}
	vals = append(vals, req.Env...)
	for _, rules := range [][]Rule{req.Permissions.Allow, req.Permissions.Ask, req.Permissions.Deny} {
		vals = append(vals, RuleStrings(rules)...)
	}
	for _, v := range vals {
		if strings.ContainsRune(v, 0) {
			return errors.New("remote ask: a NUL byte cannot cross the remote argv")
		}
	}
	return nil
}

// remoteOverlay is the inline overlay naming this process's confinement (HARNESS_COWORKER) for the far argv; "": none.
func remoteOverlay() string {
	provider, profile, _ := router.ParseCoworker(os.Getenv(router.EnvCoworker)) // a bad value confines nothing here either
	return agentconfig.InlineOverlay(provider, profile)
}

// remoteArgv is the full local ssh argv for one remote ask.
func remoteArgv(host, bin, agentName, fallback string, req RunOptions) ([]string, error) {
	script, err := remoteScript(bin, agentName, fallback, req)
	if err != nil {
		return nil, err
	}
	return remote.StreamArgs(host, script)
}

// resultFromEnvelope is the local Result for a remote result event; dropped lists the flags the local side dropped.
func resultFromEnvelope(env wire.Envelope, agentName string, dropped []string) wire.Result {
	res := wire.Result{Agent: cmp.Or(env.Agent, agentName), Status: env.Status, Provider: env.Provider, Model: env.Model, Invoked: env.Invoked,
		SessionID: env.SessionID, Answer: env.Answer, DurationMs: env.DurationMs, Dropped: append(dropped, env.Dropped...),
		SchemaUnchecked: env.SchemaUnchecked, Emulated: env.Emulated, FallbackReason: env.FallbackReason, Requested: env.Requested, RequestedWhy: env.RequestedWhy, Handoff: env.Handoff,
		Transport: env.Transport, Hops: env.Hops, Structured: env.Structured}
	if env.Usage != nil {
		res.Usage = *env.Usage
	}
	if env.Fallback != nil {
		res.Fallback = *env.Fallback
	}
	if res.Transport == nil {
		res.Transport = &wire.Transport{Attempts: 1}
	}
	return res
}

// Run streams the remote ask: events are re-emitted on sink, the remote result event becomes the Result.
func (r remoteEngine) Run(ctx context.Context, req RunOptions, sink wire.Sink) (wire.Result, error) {
	argv, err := remoteArgv(r.host, r.bin, r.agent, r.fallback, req)
	if err != nil {
		return wire.Result{Agent: r.agent, Status: wire.StatusError}, retry.Fatal(r.agent, err)
	}
	if strings.ContainsRune(req.Prompt, 0) {
		return wire.Result{Agent: r.agent, Status: wire.StatusError}, retry.Fatal(r.agent, errors.New("remote ask: a NUL byte cannot cross the framed prompt"))
	}
	dropped := remoteDropped(req)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stop := func() { cancel(nil) }
	touch, stopWatch := remoteWatchdog(req.Timeout, cancel)
	defer stopWatch()
	cmd, tail := ChildCmd(ctx, argv[0], argv[1:], nil, "", "", nil, false)
	cmd.Stderr = io.MultiWriter(cmd.Stderr, touchWriter(touch)) // the remote's retry notices arrive on stderr
	in, err := cmd.StdinPipe()
	var out io.ReadCloser
	if err == nil {
		out, err = cmd.StdoutPipe()
	}
	if err == nil {
		agentconfig.RecordOverlayDebug("remote", cmd.Env)
		err = cmd.Start()
	}
	if err != nil {
		return wire.Result{Agent: r.agent, Status: wire.StatusError, Dropped: dropped}, retry.Fatal(r.agent, fmt.Errorf("ssh: %w", err))
	}
	// stdin stays open after the prompt: its closing (below, or by a dead ssh) is the remote's cancel signal
	go func() { _, _ = io.WriteString(in, req.Prompt+"\x00") }()
	go func() { // cancelled: drop both pipes so a relay blocked on a stalled ssh returns, and the remote sees stdin close
		<-ctx.Done()
		_ = in.Close()
		_ = out.Close()
	}()
	env, streamed, sinkErr := r.relay(ctx, out, sink, stop, touch)
	_ = in.Close()
	werr := cmd.Wait()
	return r.remoteOutcome(ctx, env, streamed, sinkErr, werr, tail, dropped)
}

func remoteWatchdog(timeout time.Duration, cancel context.CancelCauseFunc) (func(), func()) {
	if timeout <= 0 {
		return func() {}, func() {}
	}
	idle := timeout + remoteGrace
	wd := newIdleTimer(idle, func() { cancel(fmt.Errorf("no remote output for %s: %w", idle, context.DeadlineExceeded)) })
	return func() { wd.Reset(idle) }, func() { wd.Stop() }
}

func (r remoteEngine) remoteOutcome(ctx context.Context, env *wire.Envelope, streamed bool, sinkErr, werr error, tail *retry.TailBuffer, dropped []string) (wire.Result, error) {
	res := wire.Result{Agent: r.agent, Status: wire.StatusError, Dropped: dropped}
	switch {
	case env != nil:
		res = resultFromEnvelope(*env, r.agent, dropped)
		if env.Error != nil {
			e := &retry.Error{Kind: wire.KindOf(env.Error.Kind), Agent: r.agent, Progressed: streamed,
				Err: wire.RemoteError{Kind: env.Error.Kind, Msg: "remote " + r.host + ": " + env.Error.Message}}
			return res, e
		}
		return res, nil
	case sinkErr != nil:
		return res, retry.Fatal(r.agent, sinkErr)
	case ctx.Err() != nil:
		return res, retry.Classify(r.agent, fmt.Errorf("ssh %s: %w", r.host, context.Cause(ctx)), streamed)
	}
	if streamed {
		res.Status = wire.StatusPartial
	}
	return res, r.noResult(werr, tail, streamed)
}

// noResult classifies an ssh exit that never delivered a result event.
func (r remoteEngine) noResult(werr error, tail *retry.TailBuffer, streamed bool) *retry.Error {
	detail := strings.TrimSpace(tail.String())
	if len(detail) > 600 {
		detail = "…" + strings.ToValidUTF8(detail[len(detail)-600:], "") // the cut may split a rune
	}
	code := -1
	if ee, ok := errors.AsType[*exec.ExitError](werr); ok {
		code = ee.ExitCode()
	}
	msg := fmt.Sprintf("ssh %s: connection failed (exit %d)", r.host, code)
	kind := retry.KindTransient
	switch {
	case code == 127: // the remote shell found nothing to exec: the task never started
		msg = fmt.Sprintf("remote %s: claude-code binary not found (%s); install it there or pass --host-bin", r.host, r.bin)
		kind = retry.KindFatal
	case code != 255:
		msg = fmt.Sprintf("remote ask on %s ended (exit %d) without a result event; does it run a claude-code with --format stream-json?", r.host, code)
		kind = retry.KindFatal
	}
	if detail != "" {
		msg += ": " + textutil.RedactSecrets(detail)
	}
	return &retry.Error{Kind: kind, Agent: r.agent, Progressed: streamed, Err: errors.New(msg)}
}

// relay reads the remote's NDJSON: events go to sink under the local dispatch id, the result event is returned,
// anything else (ssh banners, warnings) goes to the dispatch log. A dead sink stops the child via stop.
func (r remoteEngine) relay(ctx context.Context, out io.Reader, sink wire.Sink, stop, touch func()) (env *wire.Envelope, streamed bool, sinkErr error) {
	rd := bufio.NewReaderSize(out, 64<<10)
	for {
		line, rerr := rd.ReadBytes('\n')
		touch()
		env, streamed, sinkErr = r.relayLine(ctx, bytes.TrimSpace(line), sink, stop, env, streamed, sinkErr)
		if rerr != nil {
			return env, streamed, sinkErr
		}
	}
}

func (r remoteEngine) relayLine(ctx context.Context, line []byte, sink wire.Sink, stop func(), env *wire.Envelope, streamed bool, sinkErr error) (*wire.Envelope, bool, error) {
	if len(line) == 0 {
		return env, streamed, sinkErr
	}
	var ev wire.Event
	if !decodeRemoteEvent(line, &ev) {
		wire.LogEvent(ctx, "ssh", string(line))
		return env, streamed, sinkErr
	}
	if ev.Type == wire.EventResult && ev.Envelope != nil {
		return ev.Envelope, streamed, sinkErr
	}
	if ev.Type == wire.EventStart || sinkErr != nil {
		return env, streamed, sinkErr
	}
	ev.DispatchID, ev.Seq, ev.TMs = "", 0, 0
	ev.Agent = cmp.Or(ev.Agent, r.agent)
	if sinkErr = sink.Emit(ev); sinkErr != nil {
		stop()
	}
	return env, streamed || ev.Progress(), sinkErr
}

func decodeRemoteEvent(line []byte, ev *wire.Event) bool {
	return json.Unmarshal(line, ev) == nil && ev.Schema == wire.EventSchema && ev.Type != ""
}

// touchWriter calls touch on every write and discards the bytes.
type touchWriter func()

func (t touchWriter) Write(p []byte) (int, error) {
	t()
	return len(p), nil
}

// ListRemote runs `<bin> agents list --format <format>` on host and copies its stdout to out; a failure carries ssh's stderr.
func ListRemote(host, bin, format string, out io.Writer) error {
	script, err := listScript(bin, format)
	if err != nil {
		return fmt.Errorf("agents: %w", err)
	}
	argv, err := remote.StreamArgs(host, script)
	if err != nil {
		return fmt.Errorf("agents: %v", err)
	}
	ctx, stop := context.WithTimeout(context.Background(), 60*time.Second)
	defer stop()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stderr strings.Builder
	cmd.Stdout, cmd.Stderr = out, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("agents: %s: %v: %s", host, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func listScript(bin, format string) (string, error) {
	execBin, err := remoteExec(bin)
	if err != nil {
		return "", err
	}
	return execBin + " agents list --format " + textutil.ShellQuote(format), nil
}

// remoteExec is the `exec <bin>` head of a remote script, on the login PATH with a local-home bin rewritten to ~.
func remoteExec(bin string) (string, error) {
	if strings.TrimSpace(bin) == "" || strings.ContainsAny(bin, "\r\n\x00") {
		return "", fmt.Errorf("invalid remote binary %q", bin)
	}
	return remote.PathPrefix + " exec " + textutil.QuotePath(remote.HomePath(bin)), nil
}
