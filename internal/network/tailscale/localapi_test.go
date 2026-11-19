package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// serveStatusSocket answers GET /localapi/v0/status with the running fixture on a short-path unix socket.
func serveStatusSocket(t *testing.T) string {
	t.Helper()
	fixture, err := os.ReadFile("testdata/status_running.json")
	if err != nil {
		t.Fatal(err)
	}
	return serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != localAPIHost || r.Method != http.MethodGet || r.URL.Path != "/localapi/v0/status" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Write(fixture)
	}))
}

func serveUnix(t *testing.T, h http.Handler) string {
	t.Helper()
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
	go http.Serve(ln, h)
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

func TestAdvertiseExitNodeKeepsSubnets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets")
	}
	var mu sync.Mutex
	var patch []byte
	sock := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != localAPIHost || r.URL.Path != "/localapi/v0/prefs" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodGet:
			w.Write([]byte(`{"AdvertiseRoutes":["10.0.0.0/24"]}`))
		case http.MethodPatch:
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			patch = b
			mu.Unlock()
			w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(StubLocalAPI(sock))

	if err := advertiseExitNode(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	var got struct {
		AdvertiseRoutes    []string
		AdvertiseRoutesSet bool
	}
	if err := json.Unmarshal(patch, &got); err != nil {
		t.Fatalf("PATCH body %q: %v", patch, err)
	}
	want := []string{"10.0.0.0/24", "0.0.0.0/0", "::/0"}
	if !got.AdvertiseRoutesSet || !reflect.DeepEqual(got.AdvertiseRoutes, want) {
		t.Errorf("PATCH body = %s, want routes %v with AdvertiseRoutesSet", patch, want)
	}
}

func TestLoginLocalShowsURLThenRunning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets")
	}
	sock := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != localAPIHost || !strings.HasPrefix(r.URL.String(), "/localapi/v0/watch-ipn-bus") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		for _, m := range []string{`{"State":1}`, `{"BrowseToURL":"https://login.example/x"}`, `{"State":6}`} {
			io.WriteString(w, m+"\n")
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(StubLocalAPI(sock))

	var out strings.Builder
	if err := loginLocal(context.Background(), func(context.Context) error { return nil }, &out); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out.String(), "https://login.example/x"); n != 1 {
		t.Errorf("sign-in URL printed %d times, want once; out = %q", n, out.String())
	}
}
