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
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Live-session limits and wire constants.
const (
	LiveSchema      = 1
	LiveSockSuffix  = ".sock"
	MaxLiveSessions = 4 // each idle claude node process holds roughly 200-400 MB RSS
	DefaultLiveIdle = 10 * time.Minute
	liveIOTimeout   = 5 * time.Second
	statusBusy      = "busy"
	statusRefused   = "refused"
)

// ErrLiveBusy is the refusal of a second turn while one is in flight.
var ErrLiveBusy = errors.New("a turn is already in flight on this live session")

// errPeerUnsupported marks a platform with no socket peer-credential call.
var errPeerUnsupported = errors.New("peer credentials unsupported on this platform")

// LiveRequest is the one line a client sends on the session socket: a single user turn.
type LiveRequest struct {
	Schema int    `json:"schema"`
	Prompt string `json:"prompt"`
}

// LiveSocket is the unix socket path of live session id.
func LiveSocket(id string) string { return filepath.Join(DispatchDir(), id+LiveSockSuffix) }

// LiveCount counts live sessions still running, other than skip.
func LiveCount(skip string) int {
	n := 0
	for _, r := range RecentRecords(1 << 20) {
		if r.Live && r.ID != skip && r.Alive() {
			n++
		}
	}
	return n
}

// CheckLiveCap errors when starting another session (besides skip) would pass MaxLiveSessions.
func CheckLiveCap(skip string) error {
	if LiveCount(skip) >= MaxLiveSessions {
		return fmt.Errorf("%d live sessions already running (max %d): agents cancel one first", MaxLiveSessions, MaxLiveSessions)
	}
	return nil
}

// pumpBuf is an unbounded in-memory pipe: a goroutine drains claude's stdout into it for the session's life, so a
// stdin write can never block behind output nobody has read yet.
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

// AcquireLiveSlot takes one of MaxLiveSessions flock'd slot files; the kernel frees it when the holder dies, and two
// contenders for the last slot cannot both win. The returned func releases it.
func AcquireLiveSlot() (func(), error) {
	if err := os.MkdirAll(DispatchDir(), 0o700); err != nil {
		return nil, err
	}
	for i := range MaxLiveSessions {
		f, err := os.OpenFile(filepath.Join(DispatchDir(), fmt.Sprintf("live-slot-%d", i)), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		if tryLockFile(f) {
			return func() { _ = f.Close() }, nil
		}
		_ = f.Close()
	}
	return nil, fmt.Errorf("%d live sessions already running (max %d): agents cancel one first", MaxLiveSessions, MaxLiveSessions)
}

// liveSession is the holder's state: one agent child, its stdin, and one buffered reader over its stdout.
type liveSession struct {
	rec     *Record
	name    string
	be      liveBackend
	stdin   io.Writer
	dec     *bufio.Reader
	events  *EventLog
	idle    *time.Timer
	idleFor time.Duration
	busy    atomic.Bool
	cancel  func(error)
	profile string
	mu      sync.Mutex // guards session
	session string
}

// ServeLive holds rec.Agent's stream-json child open behind rec's socket until idle, cancel or the child exits.
// rec.Request.Timeout is the idle limit; the record is finalized on the way out.
func ServeLive(ctx context.Context, rec *Record, notify func(string)) (err error) {
	req, name := rec.Request, rec.Agent
	lc, ok := schemaFor(name).Live()
	if !ok {
		return fmt.Errorf("%s has no live mode (claude|gemini)", name)
	}
	be := lc.newLive()
	release, err := AcquireLiveSlot()
	if err != nil {
		return err
	}
	defer release()
	runner, err := Lookup(name)
	if err == nil {
		err = runner.Available()
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	logSink := NewLogSink(rec.LogPath())
	defer logSink.Close()
	ctx = WithLogEvents(WithLog(ctx, logSink), logSink)
	cmd, tail, profile, err := be.build(ctx, req)
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
	if err := cmd.Start(); err != nil {
		_ = ln.Close()
		_ = os.Remove(LiveSocket(rec.ID))
		return err
	}
	exited := make(chan error, 1) // buffered: the waiter never blocks on a gone reader
	go func() { exited <- cmd.Wait() }()
	events := NewEventLog(rec.EventsPath(), rec.ID)
	defer events.Close()
	s := &liveSession{rec: rec, name: name, be: be, stdin: stdin, dec: bufio.NewReader(newPumpBuf(stdout)), events: events, idleFor: cmp.Or(req.Timeout, DefaultLiveIdle),
		cancel: func(e error) { cancel(e) }, profile: profile}
	s.idle = time.AfterFunc(s.idleFor, func() { cancel(errLiveIdle) })
	_ = rec.Save() // the pid is on disk before the first send can find the record
	_ = events.Emit(Event{Type: EventStart, Agent: name, DispatchID: rec.ID, Status: "running"})

	var wg sync.WaitGroup
	go s.accept(ctx, ln, &wg)
	var cause error
	select {
	case <-ctx.Done():
		cause = context.Cause(ctx)
	case werr := <-exited:
		exited <- werr
		cause = fmt.Errorf("%s exited: %w", name, childErr(context.Background(), name, werr, tail))
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
	res := Result{Agent: s.name, Provider: s.profile, SessionID: s.session, Status: StatusOK}
	s.mu.Unlock()
	var err error
	if !errors.Is(cause, errLiveIdle) {
		err = cause
		if errors.Is(cause, context.Canceled) {
			err = errors.New("cancelled")
		}
		res.Status = StatusError
	}
	s.rec.Finish(res, err, time.Since(s.rec.Time))
	return s.rec.Save()
}

// maxSockPath is macOS's sun_path capacity (104) less the NUL; Linux allows 108.
const maxSockPath = 103

// listenLive binds the 0600 socket for id in the 0700 dispatch dir, replacing a stale socket file.
func listenLive(id string) (net.Listener, error) {
	if err := os.MkdirAll(DispatchDir(), 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(DispatchDir(), 0o700); err != nil {
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
	out  *JSONLSink
	dead bool
}

func (k *connSink) Emit(ev Event) error {
	if k.dead {
		return nil
	}
	_ = k.c.SetWriteDeadline(time.Now().Add(liveIOTimeout))
	if err := k.out.Emit(ev); err != nil {
		k.dead = true
	}
	return nil
}

// serveConn reads one LiveRequest and runs it as the session's single in-flight turn.
func (s *liveSession) serveConn(ctx context.Context, c net.Conn) {
	if uc, ok := c.(*net.UnixConn); ok { // defense in depth beside the 0600 socket: a peer of another uid is dropped
		if uid, err := peerUID(uc); !errors.Is(err, errPeerUnsupported) && (err != nil || uid != uint32(os.Getuid())) {
			return
		}
	}
	out := &connSink{c: c, out: NewJSONLSink(c, s.rec.ID)}
	refuse := func(status, text string) {
		_ = out.Emit(Event{Type: EventError, Agent: s.name, Status: status, Text: text})
	}
	_ = c.SetReadDeadline(time.Now().Add(liveIOTimeout))
	var rq LiveRequest
	if err := json.NewDecoder(bufio.NewReader(c)).Decode(&rq); err != nil || strings.TrimSpace(rq.Prompt) == "" {
		refuse(statusRefused, "send one JSON line {\"schema\":1,\"prompt\":\"...\"}")
		return
	}
	if !s.busy.CompareAndSwap(false, true) {
		refuse(statusBusy, ErrLiveBusy.Error())
		return
	}
	if ctx.Err() != nil {
		s.release()
		refuse(statusRefused, "the session is ending")
		return
	}
	s.runTurn(ctx, rq.Prompt, Multi(out, s.events))
}

// runTurn writes one user message to the agent and relays its events up to the turn's result.
func (s *liveSession) runTurn(ctx context.Context, prompt string, sink Sink) {
	begin := time.Now()
	sink = &runSink{Sink: sink, agent: s.name}
	_ = sink.Emit(Event{Type: EventStart, DispatchID: s.rec.ID, Status: "running"})
	var res Result
	err := s.be.writeTurn(s.stdin, prompt) // the idle timer keeps running until the write lands
	s.idle.Stop()
	if err == nil {
		res, err = s.be.parse(s.dec, sink, eventLogFor(ctx))
	}
	res.Agent, res.Provider, res.DurationMs = s.name, s.profile, time.Since(begin).Milliseconds()
	res.Status = statusOf(Classify(s.name, err, false))
	s.mu.Lock()
	s.session = cmp.Or(res.SessionID, s.session)
	s.mu.Unlock()
	// A lost turn result ends the session before it is freed, so a next send can't read that turn's late answer.
	if _, atBoundary := errors.AsType[resultError](err); err != nil && !atBoundary {
		s.cancel(fmt.Errorf("session stream broke: %w", err))
	}
	s.release() // before the result goes out: a client's next send must find the session free
	_ = sink.Emit(ResultEvent(NewEnvelope(res, err, s.rec.ID)))
}

// release ends the in-flight turn and restarts the idle clock.
func (s *liveSession) release() {
	s.idle.Reset(s.idleFor)
	s.busy.Store(false)
}

// SendLive sends one turn to live session id and streams that turn's events to sink until its result.
func SendLive(ctx context.Context, id, prompt string, sink Sink) (Envelope, error) {
	rec, err := LoadRecord(id)
	if err != nil {
		return Envelope{}, err
	}
	if !rec.Live {
		return Envelope{}, fmt.Errorf("dispatch %s is not a live session (start one: ask --agent claude|gemini --live)", rec.ID)
	}
	if !rec.Alive() {
		return Envelope{}, fmt.Errorf("live session %s has ended (%s); start a new one", rec.ID, rec.Status)
	}
	conn, err := (&net.Dialer{Timeout: liveIOTimeout}).DialContext(ctx, "unix", LiveSocket(rec.ID))
	if err != nil {
		return Envelope{}, fmt.Errorf("live session %s: %w (claude-code agents reap clears dead sessions)", rec.ID, err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_ = conn.SetWriteDeadline(time.Now().Add(liveIOTimeout))
	if err := json.NewEncoder(conn).Encode(LiveRequest{Schema: LiveSchema, Prompt: prompt}); err != nil {
		return Envelope{}, err
	}
	dec := json.NewDecoder(conn)
	for {
		var ev Event
		if err := dec.Decode(&ev); err != nil {
			if ctx.Err() != nil {
				return Envelope{}, ctx.Err()
			}
			return Envelope{}, fmt.Errorf("live session %s closed before the turn's result: %w", rec.ID, err)
		}
		if ev.Type == EventError && ev.Status == statusBusy {
			return Envelope{}, ErrLiveBusy
		}
		if ev.Type == EventError && ev.Status == statusRefused {
			return Envelope{}, errors.New(ev.Text)
		}
		if err := sink.Emit(ev); err != nil {
			return Envelope{}, err
		}
		if ev.Type == EventResult {
			if ev.Envelope == nil {
				return Envelope{}, errors.New("result event without an envelope")
			}
			return *ev.Envelope, nil
		}
	}
}

// liveReadyWait bounds how long `ask --live` waits for the holder to open its socket.
const liveReadyWait = 20 * time.Second

// IdleOf is the idle limit a live session runs under: the request's timeout, else the default.
func IdleOf(req RunOptions) time.Duration {
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
		if rec, err := LoadRecord(id); err == nil && rec.Status != "running" {
			return errors.New(rec.Error)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("no socket after %s (holder pid %d)", liveReadyWait, pid)
}
