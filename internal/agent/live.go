package agent

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
)

// Live-session limits and wire constants.
const (
	maxLiveSessions = 4 // each idle claude node process holds roughly 200-400 MB RSS
	defaultLiveIdle = 10 * time.Minute
	liveIOTimeout   = 5 * time.Second
	statusBusy      = "busy"
	statusRefused   = "refused"
)

// liveSession is the holder's state: one agent child, its stdin, and one buffered reader over its stdout.
type liveSession struct {
	rec     *Record
	name    string
	be      LiveBackend
	stdin   io.Writer
	dec     *bufio.Reader
	events  *wire.EventLog
	idle    *time.Timer
	idleFor time.Duration
	busy    atomic.Bool
	cancel  func(error)
	profile string
	account string     // the profile's billing account, "" on the agent's own login: the ran-on line's hop
	mu      sync.Mutex // guards session
	session string
}

// ServeLive holds rec.Agent's stream-json child open behind rec's socket until idle, cancel or exit; Timeout is idle.
func ServeLive(ctx context.Context, rec *Record) (err error) {
	req, name := rec.Request, rec.Agent
	row, _ := LookupSchema(name)
	lc, ok := row.Live()
	if !ok {
		return fmt.Errorf("%s has no live mode (claude|gemini)", name)
	}
	be := lc.NewLive()
	release, err := acquireLiveSlot()
	if err != nil {
		return err
	}
	defer release()
	ag, err := Lookup(name)
	if err == nil {
		err = ag.Available()
	}
	if err == nil {
		req, err = checkRun(ag, req) // the ask path's policy layering and refusals; the record holds the raw request
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	logSink := wire.NewLogSink(rec.LogPath())
	defer logSink.Close()
	ctx = providers.WithIdentity(ctx, cmp.Or(req.Identity, providers.IdentityHeadless))
	ctx = wire.WithLogEvents(wire.WithLog(ctx, logSink), logSink)
	cmd, tail, profile, err := be.Build(ctx, req)
	if err != nil {
		return err
	}
	return serveChild(ctx, cancel, rec, name, be, cmd, tail, profile)
}

func serveChild(ctx context.Context, cancel context.CancelCauseFunc, rec *Record, name string,
	be LiveBackend, cmd *exec.Cmd, tail *retry.TailBuffer, profile string) error {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe() // an OS pipe: claude's output never blocks while no turn is reading
	if err != nil {
		return err
	}
	agentconfig.RecordOverlayDebug(name, cmd.Env)
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1) // buffered: the waiter never blocks on a gone reader
	pump := newPumpBuf(stdout)
	go func() { // os/exec: Wait closes the pipe, so it runs only after the pump read all of stdout (or the run is cancelled)
		select {
		case <-pump.done:
		case <-ctx.Done():
		}
		exited <- cmd.Wait()
	}()
	if err := rec.Save(); err != nil { // the pid is on disk before the first send can find the record
		cancel(err) // kills the child; the waiter reaps it
		return fmt.Errorf("live session %s: record not saved: %w", rec.ID, err)
	}
	// bound after Start: a socket on disk means a running child, so a failed start is never reported ready
	ln, err := listenLive(rec.ID)
	if err != nil {
		cancel(err) // kills the child; the waiter reaps it
		return err
	}
	events := wire.NewEventLog(rec.EventsPath(), rec.ID)
	defer events.Close()
	s := &liveSession{rec: rec, name: name, be: be, stdin: stdin, dec: bufio.NewReader(pump), events: events, idleFor: IdleOf(rec.Request),
		cancel: cancel, profile: profile, account: liveAccount(name, profile)}
	s.idle = time.AfterFunc(s.idleFor, func() { cancel(errLiveIdle) })
	_ = events.Emit(wire.Event{Type: wire.EventStart, Agent: name, DispatchID: rec.ID, Status: "running"})

	return s.awaitChild(ctx, ln, exited, stdin, tail)
}

func (s *liveSession) awaitChild(ctx context.Context, ln net.Listener, exited chan error, stdin io.Closer,
	tail *retry.TailBuffer) error {
	var wg sync.WaitGroup
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		s.accept(ctx, ln, &wg)
	}()
	var cause error
	select {
	case <-ctx.Done():
		cause = context.Cause(ctx)
	case werr := <-exited:
		exited <- werr
		cause = fmt.Errorf("%s exited", s.name)
		if cerr := ChildErr(context.Background(), s.name, werr, tail); cerr != nil {
			cause = fmt.Errorf("%s exited: %w", s.name, cerr)
		}
	}
	s.idle.Stop()
	_ = ln.Close()
	s.cancel(cause) // kills the child process group via CommandContext
	<-exited        //nolint:errcheck // cancel above ended the child, so its exit error says nothing new
	_ = stdin.Close()
	<-acceptDone // accept adds to wg, so it must stop before Wait
	wg.Wait()
	_ = os.Remove(LiveSocket(s.rec.ID))
	return s.finish(cause)
}

var errLiveIdle = errors.New("idle timeout")

// liveAccount is profile's account key, or "" when profile is agentName's own login (or unreadable).
func liveAccount(agentName, profile string) string {
	reg, err := providers.Load()
	if err != nil {
		return ""
	}
	if p := reg.Providers[profile]; p != nil && p.Auth.Login != agentName {
		return p.Account()
	}
	return ""
}

// hops is the session's one hop for the ran-on line; none on the agent's own login, which reads "own login".
func (s *liveSession) hops(model string) []wire.Hop {
	if s.account == "" {
		return nil
	}
	return []wire.Hop{{Agent: s.name, Account: s.account, Profile: s.profile, Model: model}}
}

// finish records the outcome: an idle end is a normal ok, anything else an error.
func (s *liveSession) finish(cause error) error {
	s.mu.Lock()
	res := wire.Result{Agent: s.name, Provider: s.profile, SessionID: s.session, Status: wire.StatusOK, Hops: s.hops("")}
	s.mu.Unlock()
	var err, exitErr error
	if !errors.Is(cause, errLiveIdle) {
		err = cause
		if errors.Is(cause, context.Canceled) {
			err = errors.New("cancelled")
		} else {
			exitErr = cause // the child died or its stream broke: the holder exits non-zero
		}
		res.Status = wire.StatusError
	}
	s.rec.Finish(res, err, time.Since(s.rec.Time))
	return errors.Join(exitErr, s.rec.Save())
}

// release ends the in-flight turn and restarts the idle clock.
func (s *liveSession) release() {
	s.idle.Reset(s.idleFor)
	s.busy.Store(false)
}

// ErrLiveBusy is the refusal of a second turn while one is in flight.
var ErrLiveBusy = errors.New("a turn is already in flight on this live session")

// errLiveCap is the refusal when every live slot is taken.
var errLiveCap = fmt.Errorf("%d live sessions already running (max %d): agents cancel one first", maxLiveSessions, maxLiveSessions)

// LiveSocket is the unix socket path of live session id.
func LiveSocket(id string) string { return filepath.Join(paths.DispatchDir(), id+wire.LiveSockSuffix) }

// liveCount counts live sessions still running.
func liveCount() (int, error) {
	recs, err := ReadRecent(AllRecords)
	n := 0
	for _, r := range recs {
		if r.Live && r.Alive() {
			n++
		}
	}
	return n, err
}

// CheckLiveCap errors when starting another session would pass maxLiveSessions.
func CheckLiveCap() error {
	n, err := liveCount()
	if err != nil {
		return err
	}
	if n >= maxLiveSessions {
		return errLiveCap
	}
	return nil
}

// ensureDispatchDir creates the 0700 dispatch dir.
func ensureDispatchDir() error { return os.MkdirAll(paths.DispatchDir(), 0o700) }

// acquireLiveSlot takes one of maxLiveSessions flock'd slot files (freed when the holder dies); the func releases it.
func acquireLiveSlot() (func(), error) {
	if err := ensureDispatchDir(); err != nil {
		return nil, err
	}
	for i := range maxLiveSessions {
		f, err := os.OpenFile(filepath.Join(paths.DispatchDir(), fmt.Sprintf("live-slot-%d", i)), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		if paths.TryLock(f) {
			return func() { _ = f.Close() }, nil
		}
		_ = f.Close()
	}
	return nil, errLiveCap
}

// pumpBuf is an unbounded in-memory pipe draining claude stdout, so a stdin write never blocks behind unread output.
type pumpBuf struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
	err    error         // the reader's terminal error (io.EOF on a clean end), returned once buf drains
	done   chan struct{} // closed once the reader hit EOF or an error: all of stdout is in buf
}

func newPumpBuf(src io.Reader) *pumpBuf {
	b := &pumpBuf{done: make(chan struct{})}
	b.cond = sync.NewCond(&b.mu)
	go func() {
		chunk := make([]byte, 32<<10)
		for {
			n, err := src.Read(chunk)
			b.mu.Lock()
			b.buf = append(b.buf, chunk[:n]...)
			b.closed, b.err = err != nil, err
			b.cond.Broadcast()
			b.mu.Unlock()
			if err != nil {
				close(b.done)
				return
			}
		}
	}()
	return b
}

// Read blocks until data or the end of claude's stdout; the reader's real error follows the drained buffer.
func (b *pumpBuf) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.buf) == 0 && !b.closed {
		b.cond.Wait()
	}
	if len(b.buf) == 0 {
		return 0, b.err
	}
	n := copy(p, b.buf)
	b.buf = b.buf[n:]
	return n, nil
}

// SendLive sends one turn to live session id and streams that turn's events to sink until its result.
func SendLive(ctx context.Context, id, prompt string, sink wire.Sink) (wire.Envelope, error) {
	rec, err := LoadRecord(id)
	if err != nil {
		return wire.Envelope{}, err
	}
	if !rec.Live {
		return wire.Envelope{}, fmt.Errorf("dispatch %s is not a live session (start one: ask --agent claude|gemini --live)", rec.ID)
	}
	if !rec.Alive() {
		return wire.Envelope{}, fmt.Errorf("live session %s has ended (%s); start a new one", rec.ID, rec.Status)
	}
	conn, err := (&net.Dialer{Timeout: liveIOTimeout}).DialContext(ctx, "unix", LiveSocket(rec.ID))
	if err != nil {
		return wire.Envelope{}, fmt.Errorf("live session %s: %w (claude-code agents cancel --lost clears dead sessions)", rec.ID, err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_ = conn.SetWriteDeadline(time.Now().Add(liveIOTimeout))
	if err := json.NewEncoder(conn).Encode(wire.LiveRequest{Schema: wire.LiveSchema, Prompt: prompt}); err != nil {
		return wire.Envelope{}, err
	}
	return receiveTurn(ctx, rec.ID, json.NewDecoder(conn), sink)
}

func receiveTurn(ctx context.Context, id string, dec *json.Decoder, sink wire.Sink) (wire.Envelope, error) {
	for {
		var ev wire.Event
		if err := dec.Decode(&ev); err != nil {
			if ctx.Err() != nil {
				return wire.Envelope{}, ctx.Err()
			}
			return wire.Envelope{}, fmt.Errorf("live session %s closed before the turn's result: %w", id, err)
		}
		if err := refusalError(ev); err != nil {
			return wire.Envelope{}, err
		}
		if err := sink.Emit(ev); err != nil {
			return wire.Envelope{}, err
		}
		if ev.Type == wire.EventResult {
			if ev.Envelope == nil {
				return wire.Envelope{}, errors.New("result event without an envelope")
			}
			return *ev.Envelope, nil
		}
	}
}

func refusalError(ev wire.Event) error {
	if ev.Type != wire.EventError {
		return nil
	}
	switch ev.Status {
	case statusBusy:
		return ErrLiveBusy
	case statusRefused:
		return errors.New(ev.Text)
	}
	return nil
}

// liveReadyWait bounds how long `ask --live` waits for the holder to open its socket.
const liveReadyWait = 20 * time.Second

// IdleOf is the idle limit a live session runs under: the request's timeout, else the default.
func IdleOf(req RunOptions) time.Duration {
	if req.Timeout > 0 {
		return req.Timeout
	}
	return defaultLiveIdle
}

// WaitLiveReady polls for the holder's socket; it gives up early when the holder's record leaves running.
func WaitLiveReady(id string, pid int) error {
	deadline := time.Now().Add(liveReadyWait)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(LiveSocket(id)); err == nil {
			return nil
		}
		if rec, err := LoadRecord(id); err == nil && rec.Status != "running" {
			return fmt.Errorf("live holder exited before opening its socket: %s", cmp.Or(rec.Error, "status "+rec.Status))
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("no socket after %s (holder pid %d)", liveReadyWait, pid)
}

// maxSockPath is macOS's sun_path capacity (104) less the NUL; Linux allows 108.
const maxSockPath = 103

// listenLive binds the 0600 socket for id in the 0700 dispatch dir, replacing a stale socket file.
func listenLive(id string) (net.Listener, error) {
	if err := ensureDispatchDir(); err != nil {
		return nil, err
	}
	if err := os.Chmod(paths.DispatchDir(), 0o700); err != nil {
		return nil, err
	}
	sock := LiveSocket(id)
	if len(sock) > maxSockPath {
		return nil, fmt.Errorf("live socket path is %d bytes, over the %d-byte unix limit: use a shorter CLAUDE_CONFIG_DIR (%s)", len(sock), maxSockPath, sock)
	}
	_ = os.Remove(sock)
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", sock)
	if err != nil {
		return nil, fmt.Errorf("live socket %s: %w", sock, err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// Accept retry bounds: resource-pressure errors (EMFILE and kin) back off instead of ending the session.
const (
	acceptBackoffMin = 5 * time.Millisecond
	acceptBackoffMax = time.Second
)

// accept serves connections until the listener closes; transient errors retry, a fatal one ends the session.
func (s *liveSession) accept(ctx context.Context, ln net.Listener, wg *sync.WaitGroup) {
	backoff := acceptBackoffMin
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if !transientAccept(err) {
				s.cancel(fmt.Errorf("live socket accept: %w", err))
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, acceptBackoffMax)
			continue
		}
		backoff = acceptBackoffMin
		wg.Go(func() {
			defer c.Close()
			s.serveConn(ctx, c)
		})
	}
}

// transientAccept reports whether an Accept error is resource pressure that a later retry can clear.
func transientAccept(err error) bool {
	for _, e := range []syscall.Errno{syscall.EMFILE, syscall.ENFILE, syscall.ENOBUFS, syscall.ENOMEM,
		syscall.ECONNABORTED, syscall.EAGAIN} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// connSink streams events to one client; a failed write is remembered and never reaches the parser.
type connSink struct {
	c    net.Conn
	out  *wire.JSONLSink
	dead bool
}

func (k *connSink) Emit(ev wire.Event) error {
	if k.dead {
		return nil
	}
	_ = k.c.SetWriteDeadline(time.Now().Add(liveIOTimeout))
	if err := k.out.Emit(ev); err != nil {
		k.dead = true
	}
	return nil
}

// serveConn reads one wire.LiveRequest and runs it as the session's single in-flight turn.
func (s *liveSession) serveConn(ctx context.Context, c net.Conn) {
	if uc, ok := c.(*net.UnixConn); ok { // defense in depth beside the 0600 socket: a peer of another uid is dropped
		if uid, err := proc.PeerUID(uc); !errors.Is(err, proc.ErrPeerUnsupported) && (err != nil || uid != uint32(os.Getuid())) {
			return
		}
	}
	out := &connSink{c: c, out: wire.NewJSONLSink(c, s.rec.ID)}
	refuse := func(status, text string) {
		_ = out.Emit(wire.Event{Type: wire.EventError, Agent: s.name, Status: status, Text: text})
	}
	_ = c.SetReadDeadline(time.Now().Add(liveIOTimeout))
	var rq wire.LiveRequest
	if err := json.NewDecoder(bufio.NewReader(c)).Decode(&rq); err != nil || strings.TrimSpace(rq.Prompt) == "" {
		refuse(statusRefused, "send one JSON line {\"schema\":1,\"prompt\":\"...\"}")
		return
	}
	if !s.busy.CompareAndSwap(false, true) {
		refuse(statusBusy, ErrLiveBusy.Error())
		return
	}
	s.idle.Stop() // the accepted turn must not be killed by idle expiry; release restarts it
	if ctx.Err() != nil {
		s.release()
		refuse(statusRefused, "the session is ending")
		return
	}
	s.runTurn(ctx, rq.Prompt, wire.Multi(out, s.events))
}

// runTurn writes one user message to the agent and relays its events up to the turn's result.
func (s *liveSession) runTurn(ctx context.Context, prompt string, sink wire.Sink) {
	begin := time.Now()
	sink = &wire.RunSink{Sink: sink, Agent: s.name}
	_ = sink.Emit(wire.Event{Type: wire.EventStart, DispatchID: s.rec.ID, Status: "running"})
	var res wire.Result
	err := s.be.WriteTurn(s.stdin, prompt)
	if err == nil {
		res, err = s.be.Parse(s.dec, sink, wire.EventLogFor(ctx))
	}
	res.Agent, res.Provider, res.DurationMs = s.name, s.profile, time.Since(begin).Milliseconds()
	res.Hops = s.hops(res.Model)
	res.Status = StatusOf(retry.Classify(s.name, err, false))
	s.mu.Lock()
	s.session = cmp.Or(res.SessionID, s.session)
	s.mu.Unlock()
	// A lost turn result ends the session before it is freed, so a next send can't read that turn's late answer.
	if err != nil && !IsResultError(err) {
		s.cancel(fmt.Errorf("session stream broke: %w", err))
	}
	s.release() // before the result goes out: a client's next send must find the session free
	_ = sink.Emit(wire.ResultEvent(wire.NewEnvelope(res, err, s.rec.ID)))
}

// serveLive is the ServeLive seam of RunHolder.
var serveLive = ServeLive

// RunHolder is the --job child of `ask --live`: it serves rec's session until idle, cancel or the agent's exit.
func RunHolder(rec *Record, launcher Launcher) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	rec.Request.Launcher = launcher // json:"-", so the record loaded from disk has none
	if err := serveLive(ctx, rec); err != nil {
		msg := textutil.RedactSecrets(err.Error()) // stderr is the job log: the same redaction as the record
		rec.Status, rec.Error = "error", msg
		rec.DurationMs = time.Since(rec.Time).Milliseconds()
		_ = rec.Save()
		fmt.Fprintf(os.Stderr, "claude-code: ask --live: %s\n", msg)
		return 1
	}
	return 0
}
