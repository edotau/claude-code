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

	"github.com/edotau/claude-code/internal/dispatch"
)

// RemoteBinEnv names the remote claude-code binary when --remote-bin is absent; the fallback is "claude-code" on the remote PATH.
const RemoteBinEnv = "HARNESS_REMOTE_BIN"

// remoteGrace pads --timeout for the local ssh's silence watchdog (reset per remote line); the remote's verdict wins.
var remoteGrace = 30 * time.Second

// RemoteBin picks the remote binary: the flag, then HARNESS_REMOTE_BIN, then "claude-code".
func RemoteBin(flagValue string) string {
	return cmp.Or(flagValue, os.Getenv(RemoteBinEnv), "claude-code")
}

// remoteRunner runs the named agent through `claude-code ask` on another host over ssh; the remote applies the caps.
type remoteRunner struct{ agent, host, bin, fallback string }

// NewRemoteRunner wraps agentName for host; bin is the remote claude-code path (see RemoteBin), fallback the remote's policy.
func NewRemoteRunner(agentName, host, bin, fallback string) Runner {
	return remoteRunner{agentName, host, bin, fallback}
}

func (r remoteRunner) Name() string { return r.agent }

// Caps is all-native: the remote's applyCaps does the dropping, and its envelope reports what it dropped.
func (r remoteRunner) Caps() Caps {
	n := Native
	return Caps{Model: n, Provider: n, Effort: n, WorkDir: n, SessionID: n, Tools: n, NoTools: n, Autonomous: n, Memory: n,
		NoPersist: n, SafeMode: n, Direct: n, SystemPrompt: n, AppendSystemPrompt: n, Env: n, OutputSchema: n, Persona: n}
}

func (r remoteRunner) Available() error { return nil }

// remoteDropped names the flags that mean nothing on another host.
func remoteDropped(req Request) []string {
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
	if req.Persona != nil {
		out = append(out, "--persona="+req.Persona.Name)
	}
	return out
}

// remoteScript is the POSIX sh line run on the remote: every element is single-quoted, the prompt is never in it.
// --env values do ride this line, so they are visible to the remote `ps` (and, base64-wrapped, to the local one).
func remoteScript(bin, agentName, fallback string, req Request) (string, error) {
	vals := []string{bin, agentName, req.Model, req.Provider, req.Effort, req.WorkDir, req.SessionID, req.SystemPrompt,
		req.AppendSystemPrompt, req.OutputSchema}
	vals = append(vals, req.Env...)
	for _, v := range vals {
		if strings.ContainsRune(v, 0) {
			return "", errors.New("remote ask: a NUL byte cannot cross the remote argv")
		}
	}
	if strings.TrimSpace(bin) == "" || strings.ContainsAny(bin, "\r\n") {
		return "", fmt.Errorf("remote ask: invalid remote binary %q", bin)
	}
	w := []string{"exec", dispatch.QuotePath(bin), "ask", "--format", "stream-json", "--fallback", dispatch.QuoteSH(cmp.Or(fallback, FallbackNone)),
		"--stdin-framed", "--agent", dispatch.QuoteSH(agentName)}
	opt := func(flag, v string) {
		if v != "" {
			w = append(w, flag, dispatch.QuoteSH(v))
		}
	}
	opt("--model", req.Model)
	opt("--profile", req.Provider)
	opt("--effort", req.Effort)
	opt("--session", req.SessionID)
	opt("--system-prompt", req.SystemPrompt)
	opt("--append-system-prompt", req.AppendSystemPrompt)
	opt("--output-schema", req.OutputSchema)
	if req.WorkDir != "" {
		w = append(w, "--workdir", dispatch.QuotePath(req.WorkDir))
	}
	if req.Timeout > 0 {
		opt("--timeout", req.Timeout.String())
	}
	for _, kv := range req.Env {
		opt("--env", kv)
	}
	for _, b := range []struct {
		on   bool
		flag string
	}{{req.Tools, "--tools"}, {req.NoTools, "--no-tools"}, {req.Autonomous, "--autonomous"}, {req.EnableMemory, "--enable-memory"},
		{req.NoPersist, "--no-persist"}, {req.SafeMode, "--safe-mode"}, {req.Direct, "--direct"}} {
		if b.on {
			w = append(w, b.flag)
		}
	}
	return strings.Join(w, " "), nil
}

// RemoteArgv is the full local ssh argv for one remote ask.
func RemoteArgv(host, bin, agentName, fallback string, req Request) ([]string, error) {
	script, err := remoteScript(bin, agentName, fallback, req)
	if err != nil {
		return nil, err
	}
	return dispatch.StreamArgs(host, script)
}

// remoteError is a failure the remote reported in its envelope; kind is the remote's envelope token, kept verbatim.
type remoteError struct{ kind, msg string }

func (e remoteError) Error() string { return e.msg }

// remoteKind maps an envelope error token to the local failure class.
func remoteKind(token string) Kind {
	switch token {
	case "auth":
		return KindAuth
	case "rate_limit":
		return KindRateLimit
	case "transient":
		return KindTransient
	case "quota":
		return KindQuota
	}
	return KindFatal
}

// resultFromEnvelope is the local Result for a remote result event; dropped lists the flags the local side dropped.
func resultFromEnvelope(env Envelope, agentName string, dropped []string) Result {
	res := Result{Agent: cmp.Or(env.Agent, agentName), Status: env.Status, Provider: env.Provider, Model: env.Model, Invoked: env.Invoked,
		SessionID: env.SessionID, Answer: env.Answer, DurationMs: env.DurationMs, Dropped: append(dropped, env.Dropped...),
		SchemaUnchecked: env.SchemaUnchecked, Emulated: env.Emulated, FallbackReason: env.FallbackReason, Handoff: env.Handoff,
		Transport: env.Transport, Hops: env.Hops, Structured: env.Structured}
	if env.Usage != nil {
		res.Usage = *env.Usage
	}
	if env.Fallback != nil {
		res.Fallback = *env.Fallback
	}
	if res.Transport == nil {
		res.Transport = &Transport{Attempts: 1}
	}
	return res
}

// Run streams the remote ask: events are re-emitted on sink, the remote result event becomes the Result.
func (r remoteRunner) Run(ctx context.Context, req Request, sink Sink) (Result, error) {
	argv, err := RemoteArgv(r.host, r.bin, r.agent, r.fallback, req)
	if err != nil {
		return Result{Agent: r.agent, Status: StatusError}, &Error{Kind: KindFatal, Agent: r.agent, Err: err}
	}
	if strings.ContainsRune(req.Prompt, 0) {
		return Result{Agent: r.agent, Status: StatusError}, &Error{Kind: KindFatal, Agent: r.agent, Err: errors.New("remote ask: a NUL byte cannot cross the framed prompt")}
	}
	dropped := remoteDropped(req)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stop := func() { cancel(nil) }
	touch := func() {}
	if req.Timeout > 0 {
		idle := req.Timeout + remoteGrace
		wd := time.AfterFunc(idle, func() { cancel(fmt.Errorf("no remote output for %s: %w", idle, context.DeadlineExceeded)) })
		defer wd.Stop()
		touch = func() { wd.Reset(idle) }
	}
	cmd, tail := childCmd(ctx, argv[0], argv[1:], nil, "", "", nil, false)
	cmd.Stderr = io.MultiWriter(cmd.Stderr, touchWriter(touch)) // the remote's retry notices arrive on stderr
	in, err := cmd.StdinPipe()
	var out io.ReadCloser
	if err == nil {
		out, err = cmd.StdoutPipe()
	}
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		return Result{Agent: r.agent, Status: StatusError, Dropped: dropped}, &Error{Kind: KindFatal, Agent: r.agent, Err: fmt.Errorf("ssh: %w", err)}
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
	res := Result{Agent: r.agent, Status: StatusError, Dropped: dropped}
	switch {
	case env != nil:
		res = resultFromEnvelope(*env, r.agent, dropped)
		if env.Error != nil {
			e := &Error{Kind: remoteKind(env.Error.Kind), Agent: r.agent, Progressed: streamed,
				Err: remoteError{kind: env.Error.Kind, msg: "remote " + r.host + ": " + env.Error.Message}}
			return res, e
		}
		return res, nil
	case sinkErr != nil:
		return res, &Error{Kind: KindFatal, Agent: r.agent, Err: sinkErr}
	case ctx.Err() != nil:
		return res, Classify(r.agent, fmt.Errorf("ssh %s: %w", r.host, context.Cause(ctx)), streamed)
	}
	if streamed {
		res.Status = StatusPartial
	}
	return res, r.noResult(werr, tail, streamed)
}

// noResult classifies an ssh exit that never delivered a result event.
func (r remoteRunner) noResult(werr error, tail *TailBuffer, streamed bool) *Error {
	detail := strings.TrimSpace(tail.String())
	if len(detail) > 600 {
		detail = "…" + detail[len(detail)-600:]
	}
	var ee *exec.ExitError
	code := -1
	if errors.As(werr, &ee) {
		code = ee.ExitCode()
	}
	msg := fmt.Sprintf("ssh %s: connection failed (exit %d)", r.host, code)
	kind := KindTransient
	if code != 255 {
		msg = fmt.Sprintf("remote ask on %s ended (exit %d) without a result event; does it run a claude-code with --format stream-json?", r.host, code)
		kind = KindFatal
	}
	if detail != "" {
		msg += ": " + detail
	}
	return &Error{Kind: kind, Agent: r.agent, Progressed: streamed, Err: errors.New(msg)}
}

// relay reads the remote's NDJSON: events go to sink under the local dispatch id, the result event is returned,
// anything else (ssh banners, warnings) goes to the dispatch log. A dead sink stops the child via stop.
func (r remoteRunner) relay(ctx context.Context, out io.Reader, sink Sink, stop, touch func()) (env *Envelope, streamed bool, sinkErr error) {
	rd := bufio.NewReaderSize(out, 64<<10)
	for {
		line, rerr := rd.ReadBytes('\n')
		touch()
		if line = bytes.TrimSpace(line); len(line) > 0 {
			var ev Event
			if json.Unmarshal(line, &ev) != nil || ev.Schema != EventSchema || ev.Type == "" {
				logEvent(ctx, "ssh", string(line))
			} else if ev.Type == EventResult && ev.Envelope != nil {
				env = ev.Envelope
			} else if ev.Type != EventStart && sinkErr == nil {
				ev.DispatchID, ev.Seq, ev.TMs = "", 0, 0
				ev.Agent = cmp.Or(ev.Agent, r.agent)
				if sinkErr = sink.Emit(ev); sinkErr != nil {
					stop()
				}
				streamed = streamed || ev.Type == EventText
			}
		}
		if rerr != nil {
			return env, streamed, sinkErr
		}
	}
}

// touchWriter calls touch on every write and discards the bytes.
type touchWriter func()

func (t touchWriter) Write(p []byte) (int, error) {
	t()
	return len(p), nil
}
