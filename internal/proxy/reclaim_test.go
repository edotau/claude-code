package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/buildver"
)

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
