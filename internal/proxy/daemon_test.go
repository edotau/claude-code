package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/buildver"
)

// TestDaemonCommandArgs: the detached daemon re-enters the binary as `gateway proxy --serve`, not the removed `router serve`.
func TestDaemonCommandArgs(t *testing.T) {
	cmd := daemonCommand("/bin/claude-code")
	if want := []string{"/bin/claude-code", "gateway", "proxy", "--serve"}; !slices.Equal(cmd.Args, want) {
		t.Fatalf("daemon args = %v, want %v", cmd.Args, want)
	}
}

// A test binary must never bind the live port: Serve's reclaim would SIGTERM the user's running router.
func TestServeRefusesDefaultPortUnderTest(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // without the guard Serve stops at ctx.Err, so a regression fails here instead of binding the port
	if err := Serve(ctx, DefaultPort); !errors.Is(err, errTestBinary) {
		t.Fatalf("Serve(DefaultPort) under test = %v, want errTestBinary", err)
	}
}

// A router.json written at a newer schema by a live daemon must still read as running and must not be overwritten.
func TestNewerSchemaRouterJSONStaysRunning(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	doc := fmt.Sprintf(`{"schema":2,"pid":%d,"port":%d,"started":"2026-01-01T00:00:00Z","extra":"newer"}`, os.Getpid(), port)
	if err := os.MkdirAll(stateFile()[:len(stateFile())-len("/router.json")], 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFile(), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(stateFile())

	st, err := ReadState()
	if err != nil || st.PID != os.Getpid() || st.Port != port {
		t.Fatalf("ReadState = %+v, %v; want the schema-2 pid/port", st, err)
	}
	// Stop's only gate is Running; calling Stop itself would SIGTERM this test process.
	if _, ok := Running(context.Background()); !ok {
		t.Fatal("Running = false for a live pid with a healthy port; Stop would return ErrNotRunning")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	pl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	servePort := pl.Addr().(*net.TCPAddr).Port
	_ = pl.Close()
	go func() { done <- Serve(ctx, servePort) }()
	// Any HTTP response proves srv.Serve is running, which happens after Serve's router.json decision.
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", servePort))
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Serve never became ready: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}
	got, err := os.ReadFile(stateFile())
	if err != nil || string(got) != doc {
		t.Fatalf("router.json = %q, %v; want the newer build's file untouched", got, err)
	}
}

// occupy serves h on a loopback port and returns the port.
func occupy(t *testing.T, h http.HandlerFunc) int {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().(*net.TCPAddr).Port
}

// TestServeRefusesHeldPort: a held port is never swapped for a random one; the error names the holder.
func TestServeRefusesHeldPort(t *testing.T) {
	cases := []struct {
		name string
		h    http.HandlerFunc
		want string
	}{
		{"foreign", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }, "held by another process"},
		{"older router", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) }, "reports no pid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			port := occupy(t, c.h)
			err := Serve(context.Background(), port)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Serve on held :%d = %v, want %q", port, err, c.want)
			}
		})
	}
}

// TestHolderReadsPIDHeader: a current router is identified by its /healthz pid header.
func TestHolderReadsPIDHeader(t *testing.T) {
	port := occupy(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(PIDHeader, strconv.Itoa(4242))
		w.Header().Set(HeaderBuild, "previous-build")
		w.Header().Set(HeaderSessions, "3")
		w.Header().Set(HeaderInflight, "2")
		_, _ = w.Write([]byte("ok\n"))
	})
	got, ok := holder(context.Background(), port)
	if !ok || got.PID != 4242 || got.Build != "previous-build" || got.Sessions != 3 || got.Inflight != 2 {
		t.Fatalf("holder = %+v, %v; want pid 4242, build previous-build, 3 sessions, 2 in flight, healthy", got, ok)
	}
}

func TestReclaimKeepsDifferentBuildWithLiveWork(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	previousTerminate := terminateHolder
	t.Cleanup(func() { terminateHolder = previousTerminate })
	terminated := 0
	terminateHolder = func(int) error {
		terminated++
		return errors.New("unexpected termination")
	}

	for _, live := range []struct {
		name     string
		sessions string
		inflight string
	}{
		{name: "session tokens", sessions: "1", inflight: "0"},
		{name: "in-flight request", sessions: "0", inflight: "1"},
	} {
		t.Run(live.name, func(t *testing.T) {
			port := occupy(t, func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != "/healthz" {
					t.Errorf("unexpected holder probe path %q", req.URL.Path)
				}
				w.Header().Set(PIDHeader, strconv.Itoa(1<<30))
				w.Header().Set(HeaderBuild, buildver.Version()+"-other")
				w.Header().Set(HeaderSessions, live.sessions)
				w.Header().Set(HeaderInflight, live.inflight)
				_, _ = w.Write([]byte("ok\n"))
			})
			if err := reclaim(context.Background(), port, false); !errors.Is(err, errKeptHolder) {
				t.Errorf("reclaim live holder = %v, want errKeptHolder", err)
			}
			if terminated != 0 {
				t.Fatalf("reclaim attempted to terminate a holder with live work %d times", terminated)
			}
		})
	}
}

func TestReclaimRefusesHolderOfAnotherConfigDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	previousTerminate := terminateHolder
	t.Cleanup(func() { terminateHolder = previousTerminate })
	terminated := 0
	terminateHolder = func(int) error { terminated++; return nil }
	port := occupy(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(PIDHeader, strconv.Itoa(1<<30))
		w.Header().Set(HeaderConfig, "otherconfig0000")
		_, _ = w.Write([]byte("ok\n"))
	})
	if err := reclaim(context.Background(), port, false); err == nil || errors.Is(err, errKeptHolder) {
		t.Fatalf("reclaim = %v, want a refusal error", err)
	}
	if terminated != 0 {
		t.Fatal("reclaim terminated a holder of another config dir")
	}
}

// plainListener holds a loopback port with a bare TCP listener that never speaks HTTP.
func plainListener(t *testing.T) (port int, isClosed func() bool) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, func() bool {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			return true
		}
		_ = c.Close()
		return false
	}
}

func stubTerminate(t *testing.T) *[]int {
	t.Helper()
	prev := terminateHolder
	t.Cleanup(func() { terminateHolder = prev })
	var killed []int
	terminateHolder = func(pid int) error { killed = append(killed, pid); return nil }
	return &killed
}

// A holder that is not a claude-code router, or answers for another config id, is errForeignHolder and is never signalled.
func TestReclaimForeignHolderIsSentinel(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	killed := stubTerminate(t)
	tcpPort, isClosed := plainListener(t)
	otherCfg := occupy(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(PIDHeader, strconv.Itoa(1<<30))
		w.Header().Set(HeaderConfig, "otherconfig0000")
		_, _ = w.Write([]byte("ok\n"))
	})
	for name, port := range map[string]int{"plain tcp": tcpPort, "other config id": otherCfg} {
		if err := reclaim(context.Background(), port, false); !errors.Is(err, errForeignHolder) {
			t.Errorf("%s: reclaim = %v, want errForeignHolder", name, err)
		}
	}
	if len(*killed) != 0 || isClosed() {
		t.Fatalf("foreign holder touched: killed %v, listener closed %v", *killed, isClosed())
	}
}

// A stub router with our config id is still replaced, and a pid-less one is an error that is not foreign.
func TestReclaimOwnConfigStillReclaimed(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	killed := stubTerminate(t)
	own := occupy(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(PIDHeader, strconv.Itoa(1<<30)) // not a live pid: the drain wait ends at once
		w.Header().Set(HeaderConfig, configID())
		_, _ = w.Write([]byte("ok\n"))
	})
	if err := reclaim(context.Background(), own, false); err != nil {
		t.Fatalf("reclaim of our own older router = %v, want nil", err)
	}
	if len(*killed) != 1 || (*killed)[0] != 1<<30 {
		t.Fatalf("terminated %v, want [%d]", *killed, 1<<30)
	}
	pidless := occupy(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	err := reclaim(context.Background(), pidless, false)
	if err == nil || errors.Is(err, errForeignHolder) {
		t.Fatalf("pid-less holder: reclaim = %v, want a non-foreign error", err)
	}
}

// An explicit busy --port is never moved: Serve errors, binds nothing and writes no router.json.
// The DefaultPort move itself cannot run here: Serve refuses DefaultPort under go test and the const has no seam.
func TestServeExplicitBusyPortErrors(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	stubTerminate(t)
	tcpPort, _ := plainListener(t)
	pidless := occupy(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	for name, port := range map[string]int{"plain tcp": tcpPort, "pid-less router": pidless} {
		if err := Serve(context.Background(), port); err == nil {
			t.Errorf("%s: Serve on busy explicit port returned nil, want an error", name)
		}
	}
	if _, err := os.Stat(stateFile()); !os.IsNotExist(err) {
		t.Errorf("router.json written after refused Serve: %v", err)
	}
}
