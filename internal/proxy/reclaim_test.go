package proxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
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
		_, _ = w.Write([]byte("ok\n"))
	})
	if pid, ok := holder(context.Background(), port); !ok || pid != 4242 {
		t.Fatalf("holder = %d, %v; want 4242, true", pid, ok)
	}
}
