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
	"runtime"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/proc"
)

// requeueWindow bounds the whole admission exchange, including queue waits and replacement-daemon retries.
var requeueWindow = 15 * time.Second

// dialStep paces those re-dials; a var so a test can shorten it.
var dialStep = 200 * time.Millisecond

// Outcome is how an Admit ended.
type Outcome string

const (
	Admitted Outcome = "admitted" // the daemon admitted the job
	Direct   Outcome = "direct"   // no daemon, a refusal or a lost daemon: the job runs as it would without one
)

// Admit holds a detached job until the daemon admits its pool; any daemon failure returns Direct at once.
func Admit(ctx context.Context, rec agent.Record, note io.Writer) Outcome {
	if runtime.GOOS == "windows" || rec.Live || rec.Host != "" {
		return Direct
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, requeueWindow)
	defer cancel()
	for {
		out, requeue := admitOnce(ctx, rec, note)
		if !requeue {
			if out == Admitted {
				fmt.Fprintf(note, "agents serve: admitted after %s\n", time.Since(start).Round(time.Millisecond))
			}
			return out
		}
		if !redial(ctx) {
			fmt.Fprintln(note, "agents serve: daemon gone; running directly")
			return Direct
		}
	}
}

// redial shares the admission budget, including backoff between successful but unhelpful probes.
func redial(ctx context.Context) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(dialStep):
		}
		if _, err := queryStatus(ctx, time.Second); err == nil {
			return true
		}
	}
}

// admitOnce runs one admit exchange; requeue means the daemon left mid-wait and its replacement should be asked.
func admitOnce(ctx context.Context, rec agent.Record, note io.Writer) (Outcome, bool) {
	c, err := dial(ctx, time.Second)
	if err != nil {
		return Direct, false // no daemon: the job runs exactly as without one
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if err := c.SetDeadline(deadline); err != nil {
			return Direct, false
		}
	}
	if err := writeRequest(c, request{Schema: wireSchema, Op: "admit", ID: rec.ID, Parent: rec.Parent}); err != nil {
		return Direct, false
	}
	sc := bufio.NewScanner(c)
	for sc.Scan() {
		var r reply
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			return Direct, false
		}
		switch r.State {
		case StateQueued:
			fmt.Fprintf(note, "agents serve: queued on %s, %d ahead\n", r.Pool, r.Ahead)
		case StateAdmitted:
			return Admitted, false
		case StateRequeue:
			return Direct, true
		default:
			fmt.Fprintf(note, "agents serve: %s (%s); running directly\n", r.State, r.Reason)
			return Direct, false
		}
	}
	return Direct, ctx.Err() == nil // a dropped connection is a daemon that died or restarted
}

// QueryStatus asks the running daemon for its queue view.
func QueryStatus(timeout time.Duration) (Status, error) {
	return queryStatus(context.Background(), timeout)
}

func queryStatus(ctx context.Context, timeout time.Duration) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c, err := dial(ctx, timeout)
	if err != nil {
		return Status{}, err
	}
	defer c.Close()
	pid, err := peerPID(c.(*net.UnixConn))
	if err != nil || pid <= 0 {
		return Status{}, errors.New("agents serve: cannot verify status peer pid")
	}
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := c.SetDeadline(deadline); err != nil {
		return Status{}, err
	}
	if err := writeRequest(c, request{Schema: wireSchema, Op: "status"}); err != nil {
		return Status{}, err
	}
	var r reply
	if err := json.NewDecoder(io.LimitReader(c, 1<<20)).Decode(&r); err != nil {
		return Status{}, err
	}
	if r.Status == nil {
		return Status{}, fmt.Errorf("agents serve: %s %s", r.State, r.Reason)
	}
	if r.Status.PID != pid {
		return Status{}, errors.New("agents serve: status pid does not match socket peer")
	}
	return *r.Status, nil
}

// dial connects to the socket after checking it is a socket this user owns, never through a symlink.
func dial(ctx context.Context, timeout time.Duration) (net.Conn, error) {
	if runtime.GOOS == "windows" {
		return nil, ErrUnsupported
	}
	if err := ensureDir(Dir()); err != nil {
		return nil, err
	}
	if err := checkSock(SockPath()); errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotRunning
	} else if err != nil {
		return nil, err
	}
	before, err := os.Lstat(SockPath())
	if err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: timeout}
	c, err := d.DialContext(ctx, "unix", SockPath())
	if err != nil {
		return nil, err
	}
	uid, err := proc.PeerUID(c.(*net.UnixConn))
	if err != nil || uid != uint32(os.Getuid()) {
		_ = c.Close()
		return nil, errors.New("agents serve: socket peer is not this user")
	}
	if err := checkConnectedSocket(before); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

func checkConnectedSocket(before os.FileInfo) error {
	after, statErr := os.Lstat(SockPath())
	if err := checkSock(SockPath()); err != nil || statErr != nil || !os.SameFile(before, after) {
		return errors.New("agents serve: socket changed while connecting")
	}
	return nil
}

func writeRequest(c net.Conn, rq request) error {
	b, err := json.Marshal(rq)
	if err != nil {
		return err
	}
	_, err = c.Write(append(b, '\n'))
	return err
}
