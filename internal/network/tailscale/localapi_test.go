package tailscale

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// serveStatusSocket answers GET /localapi/v0/status with the running fixture on a short-path unix socket.
func serveStatusSocket(t *testing.T) string {
	t.Helper()
	fixture, err := os.ReadFile("testdata/status_running.json")
	if err != nil {
		t.Fatal(err)
	}
	d, err := os.MkdirTemp("", "ts") // macOS caps unix socket paths at 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	sock := filepath.Join(d, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != localAPIHost || r.Method != http.MethodGet || r.URL.Path != "/localapi/v0/status" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Write(fixture)
	}))
	return sock
}

func TestStatusLocalAPI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets")
	}
	sock := serveStatusSocket(t)
	prev := lookPathReal
	lookPathReal = func(string) (string, error) { return "", errors.New("not found") }
	t.Cleanup(func() { lookPathReal = prev })
	t.Cleanup(StubMacAppCLI(""))
	t.Cleanup(StubLocalAPI(sock))

	got, err := Status(context.Background())
	if err != nil || got.Via != "localapi" || !strings.HasPrefix(got.DNSName, "test-mac.tailxxxxx.ts.net") {
		t.Fatalf("Status() = %q via %q, err %v", got.DNSName, got.Via, err)
	}

	t.Cleanup(StubLocalAPI(""))
	if _, err := Status(context.Background()); !errors.Is(err, errNoCLI) {
		t.Errorf("Status() without LocalAPI err = %v, want errNoCLI", err)
	}
}
