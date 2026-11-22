package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/proxy"
)

const (
	wireSchema      = 1
	maxRequestBytes = 4 << 10
	maxConns        = MaxQueue + 16
	ioTimeout       = 5 * time.Second
	helloTimeout    = 500 * time.Millisecond
	maxSockPath     = 104 // sun_path on macOS; Linux allows 108
	dirMode         = 0o700
)

// tick paces admission while jobs wait; a var so a test can shorten it.
var tick = 500 * time.Millisecond

// Reply states a daemon sends an admit request.
const (
	StateQueued   = "queued"
	StateAdmitted = "admitted"
	StateRefused  = "refused"
	StateRequeue  = "requeue" // the daemon is draining: dial its replacement
)

// request is one client line; the job is named by id and parent only, checked against its record and the peer pid.
type request struct {
	Schema int    `json:"schema"`
	Op     string `json:"op"` // admit | status
	ID     string `json:"id,omitempty"`
	Parent string `json:"parent,omitempty"`
}

// reply is one daemon line.
type reply struct {
	State  string  `json:"state"`
	Pool   string  `json:"pool,omitempty"`
	Ahead  int     `json:"ahead,omitempty"`
	Reason string  `json:"reason,omitempty"`
	Status *Status `json:"status,omitempty"`
}

// Status is the daemon and its queue view, `agents serve --status`.
type Status struct {
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
	Cap     int       `json:"cap,omitempty"`
	Pools   []PoolRow `json:"pools"`
}

// idRE is a dispatch record id: a timestamp and an agent name, never a path.
var idRE = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}\.[0-9]{3}-[A-Za-z0-9_.-]{1,64}$`)

type daemon struct {
	q       *Queue
	st      State
	kick    chan struct{}
	conns   chan struct{}
	peerPID func(*net.UnixConn) (int, error)
}

// Serve runs the daemon in the foreground until ctx ends, then drains: every waiting job is told to re-queue.
func Serve(ctx context.Context, ceiling int) error {
	if runtime.GOOS == "windows" {
		return ErrUnsupported
	}
	if err := ensureDir(Dir()); err != nil {
		return err
	}
	lock, err := openServeFile("daemon.lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	if !paths.TryLock(lock) {
		return errors.New("agents serve: another daemon owns the listener")
	}
	return serveLocked(ctx, ceiling)
}

func serveLocked(ctx context.Context, ceiling int) error {
	ln, err := listen()
	if err != nil {
		return err
	}
	defer ln.Close()
	fi, err := os.Lstat(SockPath())
	if err != nil {
		return err
	}
	defer removeSocket(fi)
	st := State{PID: os.Getpid(), Started: time.Now().UTC(), Exe: proxy.ExeHash(), Cap: ceiling}
	cleanup, err := writeState(st)
	if err != nil {
		_ = ln.Close()
		return err
	}
	defer cleanup()
	d := &daemon{q: NewQueue(RecordSources(), ceiling), st: st, kick: make(chan struct{}, 1),
		conns: make(chan struct{}, maxConns), peerPID: peerPID}
	fmt.Fprintf(os.Stderr, "agents serve: pid %d on %s (cap %d)\n", st.PID, SockPath(), ceiling)
	var wg sync.WaitGroup
	wg.Go(func() { d.schedule(ctx) })
	context.AfterFunc(ctx, func() { _ = ln.Close() })
	d.accept(ctx, ln, &wg)
	wg.Wait()
	return nil
}

// accept serves connections until the listener closes; readRequest bounds handshakes independently of queued jobs.
func (d *daemon) accept(ctx context.Context, ln *net.UnixListener, wg *sync.WaitGroup) {
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		wg.Go(func() {
			defer c.Close()
			d.serveConn(ctx, c)
		})
	}
}

// schedule re-checks admission on every enqueue and every tick while jobs wait.
func (d *daemon) schedule(ctx context.Context) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.kick:
		case <-t.C:
		}
		d.q.Admit(time.Now())
	}
}

// serveConn reads one request from a same-uid peer and answers it.
func (d *daemon) serveConn(ctx context.Context, c *net.UnixConn) {
	rq, err := d.readRequest(c)
	if err != nil {
		return
	}
	switch rq.Op {
	case "status":
		send(c, reply{State: "status", Status: d.status()})
	case "admit":
		d.admit(ctx, c, rq)
	default:
		send(c, reply{State: StateRefused, Reason: "unknown op"})
	}
}

func (d *daemon) readRequest(c *net.UnixConn) (request, error) {
	select {
	case d.conns <- struct{}{}:
		defer func() { <-d.conns }()
	default:
		return request{}, ErrQueueFull
	}
	if uid, err := proc.PeerUID(c); err != nil || uid != uint32(os.Getuid()) {
		return request{}, errors.New("peer uid does not match")
	}
	if err := c.SetReadDeadline(time.Now().Add(helloTimeout)); err != nil {
		return request{}, err
	}
	line, err := bufio.NewReader(io.LimitReader(c, maxRequestBytes)).ReadBytes('\n')
	var rq request
	if err != nil || json.Unmarshal(line, &rq) != nil || rq.Schema != wireSchema {
		return request{}, errors.New("send one JSON line {\"schema\":1,\"op\":...}")
	}
	return rq, c.SetReadDeadline(time.Time{})
}

func (d *daemon) status() *Status {
	return &Status{PID: d.st.PID, Started: d.st.Started, Cap: d.st.Cap, Pools: d.q.View(time.Now())}
}

// admit queues the job after checking that the peer is its recorded runner, then answers once it may start.
func (d *daemon) admit(ctx context.Context, c *net.UnixConn, rq request) {
	rec, err := d.verify(c, rq)
	if err != nil {
		send(c, reply{State: StateRefused, Reason: err.Error()})
		return
	}
	pool := PoolOf(rec)
	w, ahead, err := d.q.Enqueue(rec.ID, pool, time.Now())
	if err != nil {
		send(c, reply{State: StateRefused, Pool: pool, Reason: err.Error()})
		return
	}
	if !send(c, reply{State: StateQueued, Pool: pool, Ahead: ahead}) {
		d.q.Remove(w)
		return
	}
	select {
	case d.kick <- struct{}{}:
	default:
	}
	gone := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, c); close(gone) }() // EOF: the job left the queue
	select {
	case <-w.grant:
		send(c, reply{State: StateAdmitted, Pool: pool})
	case <-gone:
		d.q.Remove(w)
	case <-ctx.Done():
		d.q.Remove(w)
		send(c, reply{State: StateRequeue, Pool: pool})
	}
}

// verify loads the named record and requires it running, with the peer as its pid and the parent it claims.
func (d *daemon) verify(c *net.UnixConn, rq request) (agent.Record, error) {
	rec, err := loadNamed(rq)
	if err != nil {
		return rec, err
	}
	pid, err := d.peerPID(c)
	switch {
	case err != nil:
		return agent.Record{}, fmt.Errorf("peer pid: %w", err)
	case pid <= 0 || rec.PID <= 0 || rec.Status != "running" || rec.PID != pid:
		return agent.Record{}, errors.New("the caller is not this dispatch's running job")
	case rec.Parent != rq.Parent:
		return agent.Record{}, errors.New("parent does not match the record")
	case rec.Live || rec.Host != "":
		return agent.Record{}, errors.New("live and --host runs are not queued")
	}
	return rec, nil
}

// loadNamed validates the request's ids and loads the job's record by its exact id.
func loadNamed(rq request) (agent.Record, error) {
	if !idRE.MatchString(rq.ID) || (rq.Parent != "" && !idRE.MatchString(rq.Parent)) {
		return agent.Record{}, errors.New("bad dispatch id")
	}
	rec, err := agent.LoadRecord(rq.ID)
	if err != nil || rec.ID != rq.ID {
		return agent.Record{}, errors.New("no such dispatch record")
	}
	return rec, nil
}

// peerCred runs read on c's file descriptor.
func peerCred(c *net.UnixConn, read func(fd int) (int, error)) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var n int
	var rerr error
	if err := raw.Control(func(fd uintptr) { n, rerr = read(int(fd)) }); err != nil {
		return 0, err
	}
	return n, rerr
}

// send writes r as one JSON line within ioTimeout; false when the client is gone.
func send(c net.Conn, r reply) bool {
	b, err := json.Marshal(r)
	if err != nil {
		return false
	}
	_ = c.SetWriteDeadline(time.Now().Add(ioTimeout))
	_, err = c.Write(append(b, '\n'))
	return err == nil
}

// listen binds the socket 0600 in its 0700 dir after the ownership checks; a stale socket of ours is replaced.
func listen() (*net.UnixListener, error) {
	if err := ensureDir(Dir()); err != nil {
		return nil, err
	}
	path := SockPath()
	if len(path) >= maxSockPath {
		return nil, fmt.Errorf("agents serve: socket path %s is over %d bytes; use a shorter CLAUDE_CONFIG_DIR", path, maxSockPath)
	}
	if err := checkSock(path); err == nil {
		if err := removeStaleSocket(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := listenPrivate(path)
	if err != nil {
		return nil, err
	}
	ln.SetUnlinkOnClose(false)
	if err := checkSock(path); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

func removeStaleSocket(path string) error {
	dialer := net.Dialer{Timeout: helloTimeout}
	c, err := dialer.DialContext(context.Background(), "unix", path)
	if err == nil {
		_ = c.Close()
		return errors.New("agents serve: another listener owns " + path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("agents serve: cannot establish socket is stale: %w", err)
	}
	return os.Remove(path)
}

func removeSocket(want os.FileInfo) {
	if cur, err := os.Lstat(SockPath()); err == nil && os.SameFile(want, cur) {
		_ = os.Remove(SockPath())
	}
}

// ensureDir makes dir 0700 and refuses one that is a symlink, not ours, or open to group or others.
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if uid, ok := paths.OwnerUID(fi); !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 || (ok && uid != os.Getuid()) {
		return fmt.Errorf("agents serve: %s is not a directory owned by this user", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("agents serve: %s is open to other users (mode %o); want 0700", dir, fi.Mode().Perm())
	}
	return nil
}

// checkSock refuses a socket path that is a symlink, not a socket, or not ours; os.ErrNotExist when absent.
func checkSock(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if uid, ok := paths.OwnerUID(fi); fi.Mode().Type() != os.ModeSocket || fi.Mode().Perm() != 0o600 || (ok && uid != os.Getuid()) {
		return fmt.Errorf("agents serve: %s is not a socket owned by this user", path)
	}
	return nil
}
