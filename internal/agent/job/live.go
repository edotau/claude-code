package job

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
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// Live-session limits and wire constants.
const (
	MaxLiveSessions = 4 // each idle claude node process holds roughly 200-400 MB RSS
	DefaultLiveIdle = 10 * time.Minute
	liveIOTimeout   = 5 * time.Second
	statusBusy      = "busy"
	statusRefused   = "refused"
)

// liveSession is the holder's state: one agent child, its stdin, and one buffered reader over its stdout.
type liveSession struct {
	rec     *Record
	name    string
	be      agent.LiveBackend
	stdin   io.Writer
	dec     *bufio.Reader
	events  *wire.EventLog
	idle    *time.Timer
	idleFor time.Duration
	busy    atomic.Bool
	cancel  func(error)
	profile string
	mu      sync.Mutex // guards session
	session string
}

// ServeLive holds rec.Agent's stream-json child open behind rec's socket until idle, cancel or exit; Timeout is idle.
func ServeLive(ctx context.Context, rec *Record) (err error) {
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
	be agent.LiveBackend, cmd *exec.Cmd, tail *retry.TailBuffer, profile string) error {
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
		cancel: cancel, profile: profile}
	s.idle = time.AfterFunc(s.idleFor, func() { cancel(errLiveIdle) })
	_ = events.Emit(wire.Event{Type: wire.EventStart, Agent: name, DispatchID: rec.ID, Status: "running"})

	return s.awaitChild(ctx, ln, exited, stdin, tail)
}

func (s *liveSession) awaitChild(ctx context.Context, ln net.Listener, exited chan error, stdin io.Closer,
	tail *retry.TailBuffer) error {
	var wg sync.WaitGroup
	go s.accept(ctx, ln, &wg)
	var cause error
	select {
	case <-ctx.Done():
		cause = context.Cause(ctx)
	case werr := <-exited:
		exited <- werr
		cause = fmt.Errorf("%s exited: %w", s.name, agent.ChildErr(context.Background(), s.name, werr, tail))
	}
	s.idle.Stop()
	_ = ln.Close()
	s.cancel(cause) // kills the child process group via CommandContext
	<-exited        //nolint:errcheck // cancel above ended the child, so its exit error says nothing new
	_ = stdin.Close()
	wg.Wait()
	_ = os.Remove(LiveSocket(s.rec.ID))
	return s.finish(cause)
}

var errLiveIdle = errors.New("idle timeout")

// finish records the outcome: an idle end is a normal ok, anything else an error.
func (s *liveSession) finish(cause error) error {
	s.mu.Lock()
	res := wire.Result{Agent: s.name, Provider: s.profile, SessionID: s.session, Status: wire.StatusOK}
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
var errLiveCap = fmt.Errorf("%d live sessions already running (max %d): agents cancel one first", MaxLiveSessions, MaxLiveSessions)

// LiveSocket is the unix socket path of live session id.
func LiveSocket(id string) string { return filepath.Join(paths.DispatchDir(), id+wire.LiveSockSuffix) }

// liveCount counts live sessions still running.
func liveCount() (int, error) {
	recs, err := ReadRecent(1 << 20)
	n := 0
	for _, r := range recs {
		if r.Live && r.Alive() {
			n++
		}
	}
	return n, err
}

// CheckLiveCap errors when starting another session would pass MaxLiveSessions.
func CheckLiveCap() error {
	n, err := liveCount()
	if err != nil {
		return err
	}
	if n >= MaxLiveSessions {
		return errLiveCap
	}
	return nil
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
		return wire.Envelope{}, fmt.Errorf("live session %s: %w (claude-code agents reap clears dead sessions)", rec.ID, err)
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
		if rec, err := LoadRecord(id); err == nil && rec.Status != "running" {
			return fmt.Errorf("live holder exited before opening its socket: %s", cmp.Or(rec.Error, "status "+rec.Status))
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("no socket after %s (holder pid %d)", liveReadyWait, pid)
}
