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

	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/proc"
)

// serveListening runs once Serve holds its port; tests wait on it instead of sleeping.
var serveListening = func() {}

// Serve runs the router on 127.0.0.1:port until ctx ends, then drains in-flight streams.
func Serve(ctx context.Context, port int) error {
	// A test binary never binds (or reclaims) the live router's port: gateway.Reclaim would SIGTERM the user's router.
	if exe, err := os.Executable(); err == nil && port == gateway.DefaultPort && proc.IsSelfUnderTest(exe) {
		return gateway.ErrTestBinary
	}
	secret, err := gateway.ClientSecret()
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

	st := gateway.State{PID: os.Getpid(), Port: ln.Addr().(*net.TCPAddr).Port, Started: time.Now().UTC(), Exe: gateway.ExeHash()}
	cleanup, err := gateway.WriteState(st)
	if err != nil {
		_ = ln.Close()
		return err
	}
	defer cleanup()

	r := newRouter(secret)
	defer r.startKeepalive(ctx)()
	srv := &http.Server{Handler: r, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
	serveListening()
	fmt.Fprintf(os.Stderr, "proxy: listening on %s (pid %d)\n", gateway.Base(st.Port), st.PID)
	return serve(ctx, srv, ln)
}

func listen(ctx context.Context, port int) (net.Listener, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if errors.Is(err, syscall.EADDRINUSE) && port != 0 {
		// Clients hold this port: replace an older router of ours; only a foreign holder of the default moves us.
		err = gateway.Reclaim(ctx, port, false)
		switch {
		case errors.Is(err, gateway.ErrKeptHolder):
			return nil, nil // the live holder keeps serving; this build exits 0 without binding
		case errors.Is(err, gateway.ErrForeignHolder) && port == gateway.DefaultPort:
			fmt.Fprintf(os.Stderr, "proxy: %v; binding a free port (recorded in proxy.json)\n", err)
			ln, err = lc.Listen(ctx, "tcp", "127.0.0.1:0")
		case err == nil:
			ln, err = lc.Listen(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
		}
	}
	if err != nil {
		return nil, err
	}
	return ln, nil
}

func serve(ctx context.Context, srv *http.Server, ln net.Listener) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	dctx, cancel := context.WithTimeout(context.Background(), gateway.ShutdownDrain)
	defer cancel()
	// A stream past the drain is closed, not an error: a slow stop is still a stop.
	if err := srv.Shutdown(dctx); errors.Is(err, context.DeadlineExceeded) {
		fmt.Fprintf(os.Stderr, "proxy: drain exceeded %s, closing the remaining streams\n", gateway.ShutdownDrain)
		_ = srv.Close()
	} else if err != nil {
		return err
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
