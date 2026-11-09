package job

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

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
	sink = agent.NewRunSink(sink, s.name)
	_ = sink.Emit(wire.Event{Type: wire.EventStart, DispatchID: s.rec.ID, Status: "running"})
	var res wire.Result
	err := s.be.WriteTurn(s.stdin, prompt)
	if err == nil {
		res, err = s.be.Parse(s.dec, sink, wire.EventLogFor(ctx))
	}
	res.Agent, res.Provider, res.DurationMs = s.name, s.profile, time.Since(begin).Milliseconds()
	res.Status = agent.StatusOf(retry.Classify(s.name, err, false))
	s.mu.Lock()
	s.session = cmp.Or(res.SessionID, s.session)
	s.mu.Unlock()
	// A lost turn result ends the session before it is freed, so a next send can't read that turn's late answer.
	if err != nil && !agent.IsResultError(err) {
		s.cancel(fmt.Errorf("session stream broke: %w", err))
	}
	s.release() // before the result goes out: a client's next send must find the session free
	_ = sink.Emit(wire.ResultEvent(wire.NewEnvelope(res, err, s.rec.ID)))
}
