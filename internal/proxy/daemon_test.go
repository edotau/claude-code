package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/textutil"
)

// TestDaemonCommandArgs checks the detached daemon re-enters gateway proxy --serve.
func TestDaemonCommandArgs(t *testing.T) {
	cmd := daemonCommand("/bin/claude-code")
	if want := []string{"/bin/claude-code", "gateway", "proxy", "--serve"}; !slices.Equal(cmd.Args, want) {
		t.Fatalf("daemon args = %v, want %v", cmd.Args, want)
	}
}

// occupy serves h on a loopback port and returns the port.
func occupy(t *testing.T, h http.HandlerFunc) int {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().(*net.TCPAddr).Port
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
	got, ok := Holder(context.Background(), port)
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
	prevComm := holderComm
	t.Cleanup(func() { holderComm = prevComm })
	holderComm = func(int) string { return "claude-code" }

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
				w.Header().Set(HeaderBuild, textutil.BuildVersion()+"-other")
				w.Header().Set(HeaderSessions, live.sessions)
				w.Header().Set(HeaderInflight, live.inflight)
				_, _ = w.Write([]byte("ok\n"))
			})
			if err := Reclaim(context.Background(), port, false); !errors.Is(err, ErrKeptHolder) {
				t.Errorf("Reclaim live holder = %v, want ErrKeptHolder", err)
			}
			if terminated != 0 {
				t.Fatalf("Reclaim attempted to terminate a holder with live work %d times", terminated)
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
	if err := Reclaim(context.Background(), port, false); err == nil || errors.Is(err, ErrKeptHolder) {
		t.Fatalf("Reclaim = %v, want a refusal error", err)
	}
	if terminated != 0 {
		t.Fatal("Reclaim terminated a holder of another config dir")
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
	prevComm := holderComm
	t.Cleanup(func() { holderComm = prevComm })
	holderComm = func(int) string { return "claude-code" }
	return &killed
}

// A /healthz reply naming a pid that is not a claude-code process is foreign: that pid is never signalled.
func TestReclaimRefusesPidOfAnotherProcess(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	killed := stubTerminate(t)
	holderComm = func(int) string { return "sshd" }
	port := occupy(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(PIDHeader, strconv.Itoa(1<<30))
		_, _ = w.Write([]byte("ok\n"))
	})
	if err := Reclaim(context.Background(), port, false); !errors.Is(err, ErrForeignHolder) || !strings.Contains(err.Error(), "sshd") {
		t.Fatalf("Reclaim = %v, want ErrForeignHolder naming sshd", err)
	}
	if len(*killed) != 0 {
		t.Fatalf("signalled %v, want nothing", *killed)
	}
}

// Foreign holders are never signalled.
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
		if err := Reclaim(context.Background(), port, false); !errors.Is(err, ErrForeignHolder) {
			t.Errorf("%s: Reclaim = %v, want ErrForeignHolder", name, err)
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
		w.Header().Set(HeaderConfig, ConfigID())
		_, _ = w.Write([]byte("ok\n"))
	})
	if err := Reclaim(context.Background(), own, false); err != nil {
		t.Fatalf("Reclaim of our own older router = %v, want nil", err)
	}
	if len(*killed) != 1 || (*killed)[0] != 1<<30 {
		t.Fatalf("terminated %v, want [%d]", *killed, 1<<30)
	}
	pidless := occupy(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	err := Reclaim(context.Background(), pidless, false)
	if err == nil || errors.Is(err, ErrForeignHolder) {
		t.Fatalf("pid-less holder: Reclaim = %v, want a non-foreign error", err)
	}
}

// A holder claiming a pid by headers alone (no config id) is signalled only when proxy.json records that pid.
func TestReclaimUnrecordedPIDIsNotSignalled(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	killed := stubTerminate(t)
	port := occupy(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(PIDHeader, strconv.Itoa(1<<30)) // not a live pid: the drain wait ends at once
		w.Header().Set(HeaderBuild, textutil.BuildVersion()+"-other")
		_, _ = w.Write([]byte("ok\n"))
	})
	if err := Reclaim(context.Background(), port, false); !errors.Is(err, ErrForeignHolder) {
		t.Fatalf("Reclaim of an unrecorded pid = %v, want ErrForeignHolder", err)
	}
	if len(*killed) != 0 {
		t.Fatalf("signalled unrecorded pid: %v", *killed)
	}
	cleanup, err := WriteState(State{PID: 1 << 30, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	if err := Reclaim(context.Background(), port, false); err != nil {
		t.Fatalf("Reclaim of the recorded pid = %v, want nil", err)
	}
	if len(*killed) != 1 {
		t.Fatalf("terminated %v, want the recorded pid once", *killed)
	}
}

// TestAwaitPortFreeCancelledCtxIsNotFree: a cancelled ctx with the listener still bound is an error, a closed port is free.
func TestAwaitPortFreeCancelledCtxIsNotFree(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := awaitPortFree(ctx, port); !errors.Is(err, context.Canceled) {
		t.Fatalf("awaitPortFree with cancelled ctx and bound port = %v, want context.Canceled", err)
	}
	_ = ln.Close()
	if err := awaitPortFree(context.Background(), port); err != nil {
		t.Fatalf("awaitPortFree on closed port = %v, want nil", err)
	}
}

// TestRunningRejectsPortAnsweredByAnotherPID: a recycled pid in proxy.json must not pass for a proxy another pid serves.
// TestRunningRejectsPidlessReplyFromNonClaudeCodePID: no pid header and a recorded pid that is not claude-code.
func TestRunningRejectsPidlessReplyFromNonClaudeCodePID(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	prevComm := holderComm
	t.Cleanup(func() { holderComm = prevComm })
	port := occupy(t, func(http.ResponseWriter, *http.Request) {})
	cleanup, err := WriteState(State{PID: os.Getpid(), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	holderComm = func(int) string { return "sshd" }
	if st, ok := Running(context.Background()); ok {
		t.Fatalf("Running = %+v, true; want false for a pid-less reply when pid %d is sshd", st, st.PID)
	}
	holderComm = func(int) string { return "claude-code" }
	if _, ok := Running(context.Background()); !ok {
		t.Fatal("Running = false; want true for a pid-less reply from an older claude-code proxy")
	}
}

func TestRunningRejectsPortAnsweredByAnotherPID(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	port := occupy(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(PIDHeader, strconv.Itoa(os.Getpid()+1))
	})
	cleanup, err := WriteState(State{PID: os.Getpid(), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	if st, ok := Running(context.Background()); ok {
		t.Fatalf("Running = %+v, true; want false: port %d is served by pid %d, not the recorded %d", st, port, os.Getpid()+1, os.Getpid())
	}
}
