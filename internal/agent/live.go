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

const (
	// MaxLive caps concurrent live holders: each idle agent process holds roughly 200-400 MB.
	MaxLive = 4
	// DefaultLiveIdle ends a live session that has gone this long without a turn.
	DefaultLiveIdle = 10 * time.Minute
	// maxTurnRequest bounds one turn request line; a larger prompt belongs in a file.
	maxTurnRequest = 16 << 20
	// maxSocketPath is the shortest sun_path limit we target (darwin: 104 including the NUL).
	maxSocketPath = 103
)

// SocketPath is the live session id's unix socket, inside the 0700 dispatch dir.
func SocketPath(id string) string { return filepath.Join(DispatchDir(), id+".sock") }

// LiveRecords lists the live sessions whose holder is still running.
func LiveRecords() []Record {
	var out []Record
	for _, r := range RecentRecords(1 << 20) {
		if r.Live && r.Alive() {
			out = append(out, r)
		}
	}
	return out
}

// LiveOpts tunes a live session; a zero Idle means DefaultLiveIdle.
type LiveOpts struct{ Idle time.Duration }

// liveSession is the holder's state: one agent process and the turn currently using it.
type liveSession struct {
	rec     *Record
	recMu   sync.Mutex
	name    string
	backend liveBackend
	child   *liveChild
	events  Sink // the dispatch's events.jsonl sink
	idleFor time.Duration
	idle    *time.Timer
	busy    atomic.Bool
	turns   atomic.Int32
	wg      sync.WaitGroup
}

// RunLive is the holder behind `ask --live`: it keeps the agent process open, serves `agents send` turns on the
// record's unix socket (this uid only) and returns when idle, cancelled or the process dies. sink is the dispatch sink.
func RunLive(ctx context.Context, rec *Record, req Request, o LiveOpts, sink Sink) (Result, error) {
	b := liveBackendFor(rec.Agent)
	if b == nil {
		return Result{}, fmt.Errorf("%s has no live mode", rec.Agent)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	child, err := b.start(ctx, req)
	if err != nil {
		return Result{}, err
	}
	exited := make(chan error, 1)
	go func() { exited <- child.cmd.Wait() }()
	l, err := listenLive(rec.ID)
	if err != nil {
		cancel()
		<-exited
		return child.res, err
	}
	s := &liveSession{rec: rec, name: rec.Agent, backend: b, child: child, events: sink, idleFor: cmp.Or(o.Idle, DefaultLiveIdle)}
	idleCh := make(chan struct{})
	s.idle = time.AfterFunc(s.idleFor, func() {
		if s.busy.CompareAndSwap(false, true) { // taking busy also refuses any turn that arrives during shutdown
			close(idleCh)
		}
	})
	if strings.TrimSpace(req.Prompt) != "" {
		s.busy.Store(true)
		s.idle.Stop()
		s.wg.Go(func() { s.turn(ctx, req.Prompt, Discard) })
	}
	go s.accept(ctx, l)

	var cause error
	var gone bool
	select {
	case <-ctx.Done():
		cause = errors.New("live session cancelled")
	case werr := <-exited:
		gone, cause = true, childErr(ctx, rec.Agent, cmp.Or(werr, errors.New("process exited")), child.tail)
	case <-idleCh:
	}
	s.idle.Stop()
	_ = l.Close()
	_ = os.Remove(SocketPath(rec.ID))
	if !gone {
		_ = child.stdin.Close() // EOF is the clean exit; the group kill is the backstop
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			cancel()
			<-exited
		}
	}
	cancel()
	s.wg.Wait()
	s.recMu.Lock()
	res := child.res
	res.Agent, res.SessionID = rec.Agent, cmp.Or(rec.SessionID, res.SessionID)
	s.recMu.Unlock()
	return res, cause
}

// listenLive binds the session socket: dir 0700, socket 0600, a stale file replaced.
func listenLive(id string) (*net.UnixListener, error) {
	dir := DispatchDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	path := SocketPath(id)
	if len(path) > maxSocketPath {
		return nil, fmt.Errorf("socket path is %d bytes, over the %d unix-socket limit: shorten CLAUDE_CONFIG_DIR", len(path), maxSocketPath)
	}
	_ = os.Remove(path)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}

func (s *liveSession) accept(ctx context.Context, l *net.UnixListener) {
	for {
		conn, err := l.AcceptUnix()
		if err != nil {
			return
		}
		s.wg.Go(func() { s.serve(ctx, conn) })
	}
}

// refuse answers a request the holder will not run.
func (s *liveSession) refuse(conn net.Conn, why string) {
	_ = NewJSONLSink(conn, s.rec.ID).Emit(Event{Type: EventError, Status: "refused", Text: why})
}

// serve handles one connection: a peer of another uid is dropped silently, else one request line is one turn.
func (s *liveSession) serve(ctx context.Context, conn *net.UnixConn) {
	defer conn.Close()
	if uid, err := peerUID(conn); err != nil || uid != uint32(os.Getuid()) {
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(io.LimitReader(conn, maxTurnRequest)).ReadBytes('\n')
	if err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	var rq struct {
		Prompt string `json:"prompt"`
	}
	if json.Unmarshal(line, &rq) != nil || strings.TrimSpace(rq.Prompt) == "" {
		s.refuse(conn, "bad request: want one JSON line {\"prompt\": \"...\"}")
		return
	}
	if !s.busy.CompareAndSwap(false, true) {
		s.refuse(conn, fmt.Sprintf("session %s has a turn in flight (turn %d); send again once it returns its result", s.rec.ID, s.turns.Load()))
		return
	}
	s.idle.Stop()
	s.turn(ctx, rq.Prompt, lossy{Sink: NewJSONLSink(conn, s.rec.ID)})
}

// lossy keeps a turn alive past its client: after one write failure it drops events instead of failing the parse.
type lossy struct{ Sink }

func (l lossy) Emit(ev Event) error {
	_ = l.Sink.Emit(ev)
	return nil
}

// turnSink stamps every event of one turn with its number.
type turnSink struct {
	Sink
	n int
}

func (t turnSink) Emit(ev Event) error {
	ev.Turn = t.n
	return t.Sink.Emit(ev)
}

// save mutates the dispatch record under the lock and writes it.
func (s *liveSession) save(f func(*Record)) {
	s.recMu.Lock()
	defer s.recMu.Unlock()
	f(s.rec)
	_ = s.rec.Save() // fail-open, like every dispatch record write
}

// turn runs one prompt through the held process and emits its events, ending in a result event with the turn's envelope.
// The caller holds busy; turn releases it and restarts the idle clock.
func (s *liveSession) turn(ctx context.Context, prompt string, client Sink) {
	n := int(s.turns.Add(1))
	defer func() {
		s.busy.Store(false)
		s.idle.Reset(s.idleFor)
	}()
	s.save(func(r *Record) { r.Turns = n })
	out := turnSink{Sink: &runSink{Sink: Multi(s.events, client), agent: s.name}, n: n}
	begin := time.Now()
	_ = out.Emit(Event{Type: EventStart, Status: "running"})
	var res Result
	err := s.backend.writeTurn(s.child.stdin, prompt)
	if err != nil {
		err = childErr(ctx, s.name, err, s.child.tail)
	} else {
		res, err = s.backend.parse(s.child.dec, out, eventLogFor(ctx))
	}
	res.Agent = s.name
	res.Provider, res.Model = cmp.Or(res.Provider, s.child.res.Provider), cmp.Or(res.Model, s.child.res.Model)
	res.DurationMs = time.Since(begin).Milliseconds()
	if res.SessionID != "" {
		s.save(func(r *Record) { r.SessionID, r.Provider, r.Model = res.SessionID, res.Provider, res.Model })
	}
	_ = out.Emit(ResultEvent(NewEnvelope(res, err, s.rec.ID)))
}

// SendTurn writes one prompt to id's holder and passes each of that turn's event lines to each until its result event.
// A refusal (a turn already in flight) comes back as the error.
func SendTurn(ctx context.Context, id, prompt string, each func(line []byte, ev Event) error) (Event, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "unix", SocketPath(id))
	if err != nil {
		return Event{}, fmt.Errorf("no live session is listening for %s (ended, idle-expired or cancelled): %w", id, err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := writeJSONLine(conn, map[string]string{"prompt": prompt}); err != nil {
		return Event{}, err
	}
	br := bufio.NewReaderSize(conn, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			if ctx.Err() != nil {
				return Event{}, ctx.Err()
			}
			return Event{}, fmt.Errorf("the holder closed the connection before the turn's result: %w", err)
		}
		var ev Event
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if ev.Type == EventError && ev.Status == "refused" {
			return ev, errors.New(ev.Text)
		}
		if err := each(line, ev); err != nil {
			return ev, err
		}
		if ev.Type == EventResult {
			return ev, nil
		}
	}
}
