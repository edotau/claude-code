package job

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
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
	if req.Identity != "" {
		ctx = providers.WithIdentity(ctx, req.Identity)
	}
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
	ln, err := listenLive(rec.ID)
	if err != nil {
		return err
	}
	agentconfig.RecordOverlayDebug(name, cmd.Env)
	if err := cmd.Start(); err != nil {
		_ = ln.Close()
		_ = os.Remove(LiveSocket(rec.ID))
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
	events := wire.NewEventLog(rec.EventsPath(), rec.ID)
	defer events.Close()
	s := &liveSession{rec: rec, name: name, be: be, stdin: stdin, dec: bufio.NewReader(pump), events: events, idleFor: IdleOf(rec.Request),
		cancel: cancel, profile: profile}
	s.idle = time.AfterFunc(s.idleFor, func() { cancel(errLiveIdle) })
	_ = rec.Save() // the pid is on disk before the first send can find the record
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
