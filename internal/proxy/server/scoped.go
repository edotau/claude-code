package server

import (
	"context"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/textutil"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// Scoped is an in-process proxy for one team and id: a loopback listener of its own, no proxy.json, no reclaim.
type Scoped struct {
	port        int
	r           *Router
	srv         *http.Server
	done        chan struct{}
	stopRefresh func()
	cleanup     func()
	once        sync.Once
}

// Open starts a scoped proxy on a free loopback port; it closes when ctx ends or Close is called.
func Open(ctx context.Context, sc proxy.Scope) (*Scoped, error) {
	secret, err := proxy.ClientSecret()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	r := newRouterWith(secret, newMemorySessionStore)
	r.scoped = true
	stopRefresh, err := acquireRefresh(ctx)
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	s := &Scoped{port: ln.Addr().(*net.TCPAddr).Port, r: r, done: make(chan struct{}),
		stopRefresh: stopRefresh, cleanup: func() {}}
	if err := s.publish(sc); err != nil {
		_ = ln.Close()
		s.stopRefresh()
		return nil, err
	}
	s.srv = &http.Server{Handler: r, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
	go func() {
		defer close(s.done)
		_ = s.srv.Serve(ln)
	}()
	go func() { // Close on the parent's end and on an unexpected Serve exit, so the file and refresh never outlive it
		select {
		case <-ctx.Done():
		case <-s.done:
		}
		s.Close()
	}()
	return s, nil
}

// refresh counts this process's servers: providers' refresh admission is process-wide, so the last one out stops it.
var refresh struct {
	sync.Mutex
	n        int
	stop     func()
	draining chan struct{} // non-nil while the last holder's stop waits on exchanges, with the mutex released
}

// acquireRefresh joins the process's refresh life; it waits out a drain in progress, or ctx.
func acquireRefresh(ctx context.Context) (func(), error) {
	refresh.Lock()
	for refresh.draining != nil {
		d := refresh.draining
		refresh.Unlock()
		select {
		case <-d:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		refresh.Lock()
	}
	defer refresh.Unlock()
	if refresh.n == 0 {
		refresh.stop = providers.BackgroundRefresh(context.Background())
	}
	refresh.n++
	return releaseRefresh, nil
}

func releaseRefresh() {
	refresh.Lock()
	if refresh.n == 0 { // a second release of one holder must not drive the count negative
		refresh.Unlock()
		return
	}
	if refresh.n--; refresh.n > 0 {
		refresh.Unlock()
		return
	}
	stop, d := refresh.stop, make(chan struct{})
	refresh.draining = d
	refresh.Unlock()
	stop()
	// stop closed the process-wide exchange admission; a cancelled life re-admits it, refreshing in line
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = providers.BackgroundRefresh(cctx)
	refresh.Lock()
	refresh.draining = nil
	refresh.Unlock()
	close(d)
}

// publish writes the scope file; an empty ID is a host that keeps no file.
func (s *Scoped) publish(sc proxy.Scope) error {
	if sc.ID == "" {
		return nil
	}
	sc.PID = os.Getpid()
	start, err := proc.StartTime(sc.PID)
	if err != nil {
		return err
	}
	sc.Start = start
	sc.Port = s.port
	sc.Build = textutil.BuildVersion()
	sc.Exe = proxy.SelfExeHash()
	sc.Started = time.Now().UTC()
	cleanup, err := proxy.WriteScope(sc)
	if err != nil {
		return err
	}
	s.cleanup = cleanup
	return nil
}

// Done is closed once the listener has stopped serving, by Close or an unexpected exit.
func (s *Scoped) Done() <-chan struct{} { return s.done }

// Close stops the proxy, drains streams and removes the scope file; it is idempotent and concurrency-safe.
func (s *Scoped) Close() {
	s.once.Do(func() {
		_ = shutdownIdle(s.srv, s.r)
		<-s.done
		drainStateWrites(s.r)
		s.stopRefresh()
		s.cleanup()
	})
}
