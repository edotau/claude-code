package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/proxy"
)

// serveListening runs once Serve holds its port; tests wait on it instead of sleeping.
var serveListening = func() {}

// Serve runs the router on 127.0.0.1:port until ctx ends, then drains in-flight streams.
func Serve(ctx context.Context, port int) error {
	// A test binary never binds (or reclaims) the live router's port: proxy.Reclaim would SIGTERM the user's router.
	if exe, err := os.Executable(); err == nil && port == proxy.DefaultPort && proc.IsSelfUnderTest(exe) {
		return proxy.ErrTestBinary
	}
	secret, err := proxy.ClientSecret()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ln, err := listen(ctx, port)
	if err != nil || ln == nil {
		return err
	}

	prev, _ := proxy.ReadState()
	st := proxy.State{PID: os.Getpid(), Port: ln.Addr().(*net.TCPAddr).Port, Started: time.Now().UTC(), Exe: proxy.ExeHash()}
	cleanup, err := proxy.WriteState(st)
	if err != nil {
		_ = ln.Close()
		return err
	}
	defer cleanup()
	proxy.RetireOlder(prev, st.Port) // a co-bound older daemon drains and exits; clients never see a closed port

	r := newRouter(secret)
	defer drainStateWrites(r) // in-flight state writes finish before the process exits, within a short bound
	stopRefresh, err := acquireRefresh(ctx)
	if err != nil {
		_ = ln.Close()
		return err
	}
	defer stopRefresh()
	defer r.startKeepalive(ctx)()
	srv := &http.Server{Handler: r, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
	serveListening()
	fmt.Fprintf(os.Stderr, "proxy: listening on %s (pid %d)\n", proxy.Base(st.Port), st.PID)
	return serve(ctx, srv, ln, r)
}

func listen(ctx context.Context, port int) (net.Listener, error) {
	lc := net.ListenConfig{Control: reusePort}
	ln, err := lc.Listen(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if errors.Is(err, syscall.EADDRINUSE) && port != 0 {
		// Clients hold this port: replace an older router of ours; only a foreign holder of the default moves us.
		err = proxy.Reclaim(ctx, port, false)
		switch {
		case errors.Is(err, proxy.ErrKeptHolder):
			return nil, nil // the live holder keeps serving; this build exits 0 without binding
		case errors.Is(err, proxy.ErrForeignHolder) && port == proxy.DefaultPort:
			fmt.Fprintf(os.Stderr, "proxy: %v; binding a free port (recorded in proxy.json)\n", err)
			ln, err = lc.Listen(ctx, "tcp", "127.0.0.1:0")
		case err == nil:
			ln, err = lc.Listen(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
		}
	}
	return ln, err
}

func serve(ctx context.Context, srv *http.Server, ln net.Listener, r *Router) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// A stream past the drain is closed, not an error: a slow stop is still a stop.
	if err := shutdownIdle(srv, r); errors.Is(err, context.DeadlineExceeded) {
		fmt.Fprintf(os.Stderr, "proxy: drain exceeded %s, closing the remaining streams\n", proxy.ShutdownDrain)
	} else if err != nil {
		return err
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// idleGrace is how long nothing may be in flight before shutdownIdle cuts the connections still open.
const idleGrace = time.Second

// shutdownIdle drains srv within ShutdownDrain, closing early once r is idle for idleGrace.
func shutdownIdle(srv *http.Server, r *Router) error {
	dctx, cancel := context.WithTimeout(context.Background(), proxy.ShutdownDrain)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Shutdown(dctx) }()
	tick := time.NewTicker(idleGrace / 20)
	defer tick.Stop()
	var idleSince time.Time
	for {
		select {
		case err := <-done:
			if errors.Is(err, context.DeadlineExceeded) {
				_ = srv.Close()
			}
			return err
		case <-tick.C:
			if r.inflight.Load() != 0 {
				idleSince = time.Time{}
			} else if idleSince.IsZero() {
				idleSince = time.Now()
			} else if time.Since(idleSince) >= idleGrace {
				_ = srv.Close()
				return <-done
			}
		}
	}
}
