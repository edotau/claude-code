package live

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
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"

	"github.com/edotau/claude-code/internal/agent"
)

// Live-session limits and wire constants.
const (
	MaxLiveSessions = 4 // each idle claude node process holds roughly 200-400 MB RSS
	DefaultLiveIdle = 10 * time.Minute
	liveIOTimeout   = 5 * time.Second
	statusBusy      = "busy"
	statusRefused   = "refused"
)

// ErrLiveBusy is the refusal of a second turn while one is in flight.
var ErrLiveBusy = errors.New("a turn is already in flight on this live session")

// errLiveCap is the refusal when every live slot is taken.
var errLiveCap = fmt.Errorf("%d live sessions already running (max %d): agents cancel one first", MaxLiveSessions, MaxLiveSessions)

// LiveSocket is the unix socket path of live session id.
func LiveSocket(id string) string { return filepath.Join(paths.DispatchDir(), id+agent.LiveSockSuffix) }

// LiveCount counts live sessions still running, other than skip.
func LiveCount(skip string) int {
	n := 0
	for _, r := range agent.RecentRecords(1 << 20) {
		if r.Live && r.ID != skip && r.Alive() {
			n++
		}
	}
	return n
}

// CheckLiveCap errors when starting another session (besides skip) would pass MaxLiveSessions.
func CheckLiveCap(skip string) error {
	if LiveCount(skip) >= MaxLiveSessions {
		return errLiveCap
	}
	return nil
}

// pumpBuf is an unbounded in-memory pipe draining claude stdout, so a stdin write never blocks behind unread output.
type pumpBuf struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
}

func newPumpBuf(src io.Reader) *pumpBuf {
	b := &pumpBuf{}
	b.cond = sync.NewCond(&b.mu)
	go func() {
		chunk := make([]byte, 32<<10)
		for {
			n, err := src.Read(chunk)
			b.mu.Lock()
			b.buf = append(b.buf, chunk[:n]...)
			b.closed = err != nil
			b.cond.Broadcast()
			b.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return b
}

// Read blocks until data or the end of claude's stdout.
func (b *pumpBuf) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.buf) == 0 && !b.closed {
		b.cond.Wait()
	}
	if len(b.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.buf)
	b.buf = b.buf[n:]
	return n, nil
}

// ensureDispatchDir creates the 0700 dispatch dir.
func ensureDispatchDir() error { return os.MkdirAll(paths.DispatchDir(), 0o700) }

// AcquireLiveSlot takes one of MaxLiveSessions flock'd slot files (freed when the holder dies); the func releases it.
func AcquireLiveSlot() (func(), error) {
	if err := ensureDispatchDir(); err != nil {
		return nil, err
	}
	for i := range MaxLiveSessions {
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

// liveSession is the holder's state: one agent child, its stdin, and one buffered reader over its stdout.
type liveSession struct {
	rec     *agent.Record
	name    string
	be      agent.LiveBackend
	stdin   io.Writer
	dec     *bufio.Reader
	events  *agent.EventLog
	idle    *time.Timer
	idleFor time.Duration
	busy    atomic.Bool
	cancel  func(error)
	profile string
	mu      sync.Mutex // guards session
	session string
}

// ServeLive holds rec.Agent's stream-json child open behind rec's socket until idle, cancel or exit; Timeout is idle.
func ServeLive(ctx context.Context, rec *agent.Record) (err error) {
	req, name := rec.Request, rec.Agent
	row, _ := agent.LookupSchema(name)
	lc, ok := row.Live()
	if !ok {
		return fmt.Errorf("%s has no live mode (claude|gemini)", name)
	}
	be := lc.NewLive()
	release, err := AcquireLiveSlot()
	if err != nil {
		return err
	}
	defer release()
	ag, err := agent.Lookup(name)
	if err == nil {
		err = ag.Available()
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	logSink := agent.NewLogSink(rec.LogPath())
	defer logSink.Close()
	ctx = agent.WithLogEvents(agent.WithLog(ctx, logSink), logSink)
	cmd, tail, profile, err := be.Build(ctx, req)
	if err != nil {
		return err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe() // an OS pipe: claude's output never blocks while no turn is reading
	if err != nil {
		return err
	}
	ln, err := listenLive(rec.ID)
	if err != nil {
		return err
	}
	agent.RecordOverlayDebug(name, cmd.Env)
	if err := cmd.Start(); err != nil {
		_ = ln.Close()
		_ = os.Remove(LiveSocket(rec.ID))
		return err
	}
	exited := make(chan error, 1) // buffered: the waiter never blocks on a gone reader
	go func() { exited <- cmd.Wait() }()
	events := agent.NewEventLog(rec.EventsPath(), rec.ID)
	defer events.Close()
	s := &liveSession{rec: rec, name: name, be: be, stdin: stdin, dec: bufio.NewReader(newPumpBuf(stdout)), events: events, idleFor: IdleOf(req),
		cancel: cancel, profile: profile}
	s.idle = time.AfterFunc(s.idleFor, func() { cancel(errLiveIdle) })
	_ = rec.Save() // the pid is on disk before the first send can find the record
	_ = events.Emit(agent.Event{Type: agent.EventStart, Agent: name, DispatchID: rec.ID, Status: "running"})

	var wg sync.WaitGroup
	go s.accept(ctx, ln, &wg)
	var cause error
	select {
	case <-ctx.Done():
		cause = context.Cause(ctx)
	case werr := <-exited:
		exited <- werr
		cause = fmt.Errorf("%s exited: %w", name, agent.ChildErr(context.Background(), name, werr, tail))
	}
	s.idle.Stop()
	_ = ln.Close()
	cancel(cause) // kills the child process group via CommandContext
	<-exited      //nolint:errcheck // cancel above ended the child, so its exit error says nothing new
	_ = stdin.Close()
	wg.Wait()
	_ = os.Remove(LiveSocket(rec.ID))
	return s.finish(cause)
}

var errLiveIdle = errors.New("idle timeout")

// finish records the outcome: an idle end is a normal ok, anything else an error.
func (s *liveSession) finish(cause error) error {
	s.mu.Lock()
	res := agent.Result{Agent: s.name, Provider: s.profile, SessionID: s.session, Status: agent.StatusOK}
	s.mu.Unlock()
	var err error
	if !errors.Is(cause, errLiveIdle) {
		err = cause
		if errors.Is(cause, context.Canceled) {
			err = errors.New("cancelled")
		}
		res.Status = agent.StatusError
	}
	s.rec.Finish(res, err, time.Since(s.rec.Time))
	return s.rec.Save()
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

// accept serves connections until the listener closes; wg counts the in-flight handlers.
func (s *liveSession) accept(ctx context.Context, ln net.Listener, wg *sync.WaitGroup) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		wg.Go(func() {
			defer c.Close()
			s.serveConn(ctx, c)
		})
	}
}

// connSink streams events to one client; a failed write is remembered and never reaches the parser.
type connSink struct {
	c    net.Conn
	out  *agent.JSONLSink
	dead bool
}

func (k *connSink) Emit(ev agent.Event) error {
	if k.dead {
		return nil
	}
	_ = k.c.SetWriteDeadline(time.Now().Add(liveIOTimeout))
	if err := k.out.Emit(ev); err != nil {
		k.dead = true
	}
	return nil
}

// serveConn reads one agent.LiveRequest and runs it as the session's single in-flight turn.
func (s *liveSession) serveConn(ctx context.Context, c net.Conn) {
	if uc, ok := c.(*net.UnixConn); ok { // defense in depth beside the 0600 socket: a peer of another uid is dropped
		if uid, err := proc.PeerUID(uc); !errors.Is(err, proc.ErrPeerUnsupported) && (err != nil || uid != uint32(os.Getuid())) {
			return
		}
	}
	out := &connSink{c: c, out: agent.NewJSONLSink(c, s.rec.ID)}
	refuse := func(status, text string) {
		_ = out.Emit(agent.Event{Type: agent.EventError, Agent: s.name, Status: status, Text: text})
	}
	_ = c.SetReadDeadline(time.Now().Add(liveIOTimeout))
	var rq agent.LiveRequest
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
	s.runTurn(ctx, rq.Prompt, agent.Multi(out, s.events))
}

// runTurn writes one user message to the agent and relays its events up to the turn's result.
func (s *liveSession) runTurn(ctx context.Context, prompt string, sink agent.Sink) {
	begin := time.Now()
	sink = agent.NewRunSink(sink, s.name)
	_ = sink.Emit(agent.Event{Type: agent.EventStart, DispatchID: s.rec.ID, Status: "running"})
	var res agent.Result
	err := s.be.WriteTurn(s.stdin, prompt)
	if err == nil {
		res, err = s.be.Parse(s.dec, sink, agent.EventLogFor(ctx))
	}
	res.Agent, res.Provider, res.DurationMs = s.name, s.profile, time.Since(begin).Milliseconds()
	res.Status = agent.StatusOf(agent.Classify(s.name, err, false))
	s.mu.Lock()
	s.session = cmp.Or(res.SessionID, s.session)
	s.mu.Unlock()
	// A lost turn result ends the session before it is freed, so a next send can't read that turn's late answer.
	if err != nil && !agent.IsResultError(err) {
		s.cancel(fmt.Errorf("session stream broke: %w", err))
	}
	s.release() // before the result goes out: a client's next send must find the session free
	_ = sink.Emit(agent.ResultEvent(agent.NewEnvelope(res, err, s.rec.ID)))
}

// release ends the in-flight turn and restarts the idle clock.
func (s *liveSession) release() {
	s.idle.Reset(s.idleFor)
	s.busy.Store(false)
}

// SendLive sends one turn to live session id and streams that turn's events to sink until its result.
func SendLive(ctx context.Context, id, prompt string, sink agent.Sink) (agent.Envelope, error) {
	rec, err := agent.LoadRecord(id)
	if err != nil {
		return agent.Envelope{}, err
	}
	if !rec.Live {
		return agent.Envelope{}, fmt.Errorf("dispatch %s is not a live session (start one: ask --agent claude|gemini --live)", rec.ID)
	}
	if !rec.Alive() {
		return agent.Envelope{}, fmt.Errorf("live session %s has ended (%s); start a new one", rec.ID, rec.Status)
	}
	conn, err := (&net.Dialer{Timeout: liveIOTimeout}).DialContext(ctx, "unix", LiveSocket(rec.ID))
	if err != nil {
		return agent.Envelope{}, fmt.Errorf("live session %s: %w (claude-code agents reap clears dead sessions)", rec.ID, err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_ = conn.SetWriteDeadline(time.Now().Add(liveIOTimeout))
	if err := json.NewEncoder(conn).Encode(agent.LiveRequest{Schema: agent.LiveSchema, Prompt: prompt}); err != nil {
		return agent.Envelope{}, err
	}
	dec := json.NewDecoder(conn)
	for {
		var ev agent.Event
		if err := dec.Decode(&ev); err != nil {
			if ctx.Err() != nil {
				return agent.Envelope{}, ctx.Err()
			}
			return agent.Envelope{}, fmt.Errorf("live session %s closed before the turn's result: %w", rec.ID, err)
		}
		if ev.Type == agent.EventError && ev.Status == statusBusy {
			return agent.Envelope{}, ErrLiveBusy
		}
		if ev.Type == agent.EventError && ev.Status == statusRefused {
			return agent.Envelope{}, errors.New(ev.Text)
		}
		if err := sink.Emit(ev); err != nil {
			return agent.Envelope{}, err
		}
		if ev.Type == agent.EventResult {
			if ev.Envelope == nil {
				return agent.Envelope{}, errors.New("result event without an envelope")
			}
			return *ev.Envelope, nil
		}
	}
}

// liveReadyWait bounds how long `ask --live` waits for the holder to open its socket.
const liveReadyWait = 20 * time.Second

// IdleOf is the idle limit a live session runs under: the request's timeout, else the default.
func IdleOf(req agent.RunOptions) time.Duration {
	if req.Timeout > 0 {
		return req.Timeout
	}
	return DefaultLiveIdle
}

// WaitLiveReady polls for the holder's socket; it gives up early when the holder's record leaves running.
func WaitLiveReady(id string, pid int) error {
	deadline := time.Now().Add(liveReadyWait)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(LiveSocket(id)); err == nil {
			return nil
		}
		if rec, err := agent.LoadRecord(id); err == nil && rec.Status != "running" {
			return errors.New(rec.Error)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("no socket after %s (holder pid %d)", liveReadyWait, pid)
}

// serveLive is the ServeLive seam of RunHolder.
var serveLive = ServeLive

// RunHolder is the --job child of `ask --live`: it serves rec's session until idle, cancel or the agent's exit.
func RunHolder(rec *agent.Record, launcher agent.Launcher) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	rec.Request.Launcher = launcher // json:"-", so the record loaded from disk has none
	if err := serveLive(ctx, rec); err != nil {
		rec.Status, rec.Error = "error", err.Error()
		_ = rec.Save()
		fmt.Fprintf(os.Stderr, "claude-code: ask --live: %v\n", err)
		return 1
	}
	return 0
}
