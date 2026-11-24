package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/textutil"
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
	r := newRouter(secret)
	r.sessions = newMemorySessionStore(secret, func() time.Time { return r.now() })
	s := &Scoped{port: ln.Addr().(*net.TCPAddr).Port, r: r, done: make(chan struct{}),
		stopRefresh: acquireRefresh(), cleanup: func() {}}
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

// refresh counts live scoped proxies: providers' refresh admission is process-wide, so the last one out stops it.
var refresh struct {
	sync.Mutex
	n    int
	stop func()
}

func acquireRefresh() func() {
	refresh.Lock()
	defer refresh.Unlock()
	if refresh.n == 0 {
		refresh.stop = providers.BackgroundRefresh(context.Background())
	}
	refresh.n++
	return func() {
		refresh.Lock()
		defer refresh.Unlock()
		if refresh.n--; refresh.n == 0 {
			refresh.stop()
		}
	}
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
	sc.Started = time.Now().UTC()
	cleanup, err := proxy.WriteScope(sc)
	if err != nil {
		return err
	}
	s.cleanup = cleanup
	return nil
}

// Base is the proxy's base URL.
func (s *Scoped) Base() string { return proxy.Base(s.port) }

// Close stops the proxy, drains streams and removes the scope file; it is idempotent and concurrency-safe.
func (s *Scoped) Close() {
	s.once.Do(func() {
		dctx, cancel := context.WithTimeout(context.Background(), proxy.ShutdownDrain)
		defer cancel()
		if err := s.srv.Shutdown(dctx); errors.Is(err, context.DeadlineExceeded) {
			_ = s.srv.Close()
		}
		<-s.done
		drainStateWrites(s.r)
		s.stopRefresh()
		s.cleanup()
	})
}
