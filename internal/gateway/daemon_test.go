package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/network/egress"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
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
	prevComm := HolderComm
	t.Cleanup(func() { HolderComm = prevComm })
	HolderComm = func(int) string { return "claude-code" }

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
	prevComm := HolderComm
	t.Cleanup(func() { HolderComm = prevComm })
	HolderComm = func(int) string { return "claude-code" }
	return &killed
}

// A /healthz reply naming a pid that is not a claude-code process is foreign: that pid is never signalled.
func TestReclaimRefusesPidOfAnotherProcess(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	killed := stubTerminate(t)
	HolderComm = func(int) string { return "sshd" }
	port := occupy(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(PIDHeader, strconv.Itoa(1<<30))
		_, _ = w.Write([]byte("ok\n"))
	})
	if err := Reclaim(context.Background(), port, false); !errors.Is(err, ErrForeignHolder) ||
		!strings.Contains(err.Error(), "sshd") {
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

// TestAwaitPortFreeCancelledCtxIsNotFree: a cancelled ctx with the port still bound errors; a closed port is free.
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

// TestRunningRejectsPidlessReplyFromNonClaudeCodePID: no pid header and a recorded pid that is not claude-code.
func TestRunningRejectsPidlessReplyFromNonClaudeCodePID(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	prevComm := HolderComm
	t.Cleanup(func() { HolderComm = prevComm })
	port := occupy(t, func(http.ResponseWriter, *http.Request) {})
	cleanup, err := WriteState(State{PID: os.Getpid(), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	HolderComm = func(int) string { return "sshd" }
	if st, ok := Running(context.Background()); ok {
		t.Fatalf("Running = %+v, true; want false for a pid-less reply when pid %d is sshd", st, st.PID)
	}
	HolderComm = func(int) string { return "claude-code" }
	if _, ok := Running(context.Background()); !ok {
		t.Fatal("Running = false; want true for a pid-less reply from an older claude-code proxy")
	}
}

// TestRunningRejectsForeignServerEchoingRecordedPID: /healthz is unauthenticated, so an echoed pid proves nothing.
func TestRunningRejectsForeignServerEchoingRecordedPID(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	prevComm := HolderComm
	t.Cleanup(func() { HolderComm = prevComm })
	HolderComm = func(int) string { return "sshd" }
	port := occupy(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(PIDHeader, strconv.Itoa(os.Getpid()))
	})
	cleanup, err := WriteState(State{PID: os.Getpid(), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	if st, ok := Running(context.Background()); ok {
		t.Fatalf("Running = %+v, true; want false: the echoed pid %d is sshd, not claude-code", st, st.PID)
	}
}

// TestRunningRejectsPortAnsweredByAnotherPID: a recycled pid in proxy.json never passes for another pid's proxy.
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
		t.Fatalf("Running = %+v, true; want false: port %d is served by pid %d, not the recorded %d", st, port,
			os.Getpid()+1, os.Getpid())
	}
}

func TestWriteStateSerializesOwnershipCheckAndPublication(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	done := make(chan error, 1)
	owner := State{PID: os.Getpid(), Port: 19001}
	err := paths.WithFileLock(stateFile(), func() error {
		go func() {
			_, err := WriteState(State{PID: 1 << 30, Port: 19002})
			done <- err
		}()
		select {
		case err := <-done:
			return fmt.Errorf("WriteState bypassed the shared publication lock: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		data := fmt.Sprintf(`{"schema":1,"pid":%d,"port":%d}`, owner.PID, owner.Port)
		return paths.AtomicWrite(stateFile(), []byte(data), 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := ReadState()
	if err != nil || got.PID != owner.PID || got.Port != owner.Port {
		t.Fatalf("competing proxy clobbered owner: %+v, %v; want %+v", got, err, owner)
	}
}

// TestMain runs in the hermetic host (hosttest), so no test touches the live vault or secret.
func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

// terminate stops a real child, alive sees it gone, and awaitExit returns at once rather than polling out its deadline.
func TestAwaitExitObservesTerminatedProcess(t *testing.T) {
	argv := []string{"sleep", "30"}
	if runtime.GOOS == "windows" {
		argv = []string{"ping.exe", "-n", "30", "127.0.0.1"}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			if err := cmd.Process.Kill(); err != nil {
				t.Error(err)
			}
			_ = cmd.Wait()
		}
	})
	pid := cmd.Process.Pid
	if !alive(pid) {
		t.Fatal("running child process reported dead")
	}
	if err := terminate(pid); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("terminated child unexpectedly exited successfully")
	}
	previous := ShutdownDrain
	ShutdownDrain = -4 * time.Second
	t.Cleanup(func() { ShutdownDrain = previous })
	if !awaitExit(pid) {
		t.Fatal("awaitExit still reports a terminated process alive")
	}
}

func TestGatewayRunEgress(t *testing.T) {
	t.Setenv(egress.EnvHost, "launcher-host")
	ctx := proc.WithValues(t.Context(), proc.ValuesFromEnv(os.Environ()).With(egress.EnvHost, "run-host"))
	base := Base(DefaultPort)
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"run", ctx, base + "/via/run-host"},
		{"request", egress.WithHost(ctx, "request-host"), base + "/via/request-host"},
		{"direct", egress.WithHost(ctx, ""), base},
		{"empty snapshot", proc.WithValues(ctx, proc.Values{}), base},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ViaBaseContext(tc.ctx, base); got != tc.want {
				t.Fatalf("base = %q, want %q", got, tc.want)
			}
			t.Logf("base = %s", tc.want)
		})
	}
	if runtime.GOOS == "windows" {
		if _, ok := proc.ValuesFromEnv(daemonCommand("claude-code.exe").Env)[egress.EnvHost]; ok {
			t.Fatal("daemon inherits egress")
		}
		return
	}
	cmd := daemonCommand("/bin/sh")
	cmd.Args = []string{"sh", "-c", `if [ "${HARNESS_EGRESS_HOST+x}" = x ]; then exit 9; fi; printf scrubbed`}
	out, err := cmd.Output()
	if err != nil || string(out) != "scrubbed" {
		t.Fatalf("daemon child = %q, %v", out, err)
	}
	if got := os.Getenv(egress.EnvHost); got != "launcher-host" {
		t.Fatalf("parent host changed: %q", got)
	}
	t.Log("daemon child: egress absent; parent: launcher-host")
}
