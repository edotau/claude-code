package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"testing"
)

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
