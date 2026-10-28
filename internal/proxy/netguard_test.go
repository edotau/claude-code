package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// rejectLocal must stop every non-public resolved address and pass a public one.
func TestRejectLocal(t *testing.T) {
	for addr, wantErr := range map[string]bool{
		"127.0.0.1:80": true, "10.1.2.3:443": true, "192.168.1.1:443": true, "172.16.0.9:443": true,
		"169.254.169.254:80": true, "[::1]:443": true, "0.0.0.0:80": true, "8.8.8.8:443": false, "[2606:4700::1]:443": false,
	} {
		err := rejectLocal("tcp", addr, nil)
		if (err != nil) != wantErr {
			t.Errorf("rejectLocal(%s): err=%v, want error=%v", addr, err, wantErr)
		}
	}
}

// A literal loopback base_url is the user's choice and dials; the same server behind a hostname that
// resolves to loopback is refused before the connect.
func TestGuardedClientLiteralVsResolved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	c := guardedClient()
	resp, err := c.Do(testRequest(t, http.MethodGet, srv.URL, nil))
	if err != nil {
		t.Fatalf("literal loopback: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 204 {
		t.Fatalf("literal loopback: status=%d", resp.StatusCode)
	}
	for _, host := range []string{"localhost", "api.localhost"} {
		if !literalLocal(host) {
			t.Errorf("literalLocal(%q) = false", host)
		}
	}
	port := strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
	localResp, err := c.Do(testRequest(t, http.MethodGet, "http://localhost.:"+port, nil)) // trailing dot is still literal
	if err != nil {
		t.Errorf("localhost. must dial: %v", err)
	} else {
		_ = localResp.Body.Close()
	}
	// The guarded dialer is what a non-literal name reaches; its Control refuses the loopback it resolved to.
	_, err = guardedDialer.DialContext(context.Background(), "tcp", "127.0.0.1:"+port)
	if err == nil || !strings.Contains(err.Error(), "resolves into the local network") {
		t.Errorf("guarded dialer must refuse a loopback target: %v", err)
	}
}

// resolveProxyHosts reads HTTP(S)_PROXY through http.ProxyFromEnvironment, which net/http caches
// process-wide behind a sync.Once; t.Setenv in this process would not be seen, so this runs the
// read in a fresh subprocess per case, the way net/http's own tests exercise proxy env vars.
func TestResolveProxyHosts(t *testing.T) {
	if os.Getenv("GO_WANT_RESOLVEPROXYHOSTS_HELPER") == "1" {
		hosts := resolveProxyHosts()
		fmt.Print(hosts["proxy.corp"], " ", len(hosts))
		return
	}
	clearProxyEnv := []string{"HTTP_PROXY=", "HTTPS_PROXY=", "http_proxy=", "https_proxy=", "NO_PROXY=", "no_proxy="}
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"https proxy host recorded", append(clearProxyEnv, "HTTPS_PROXY=http://proxy.corp:3128"), "true 1"},
		{"no proxy configured", clearProxyEnv, "false 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestResolveProxyHosts$", "-test.v")
			cmd.Env = append(append([]string{}, os.Environ()...), "GO_WANT_RESOLVEPROXYHOSTS_HELPER=1")
			cmd.Env = append(cmd.Env, tc.env...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("helper process: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Errorf("resolveProxyHosts() helper output = %q, want to contain %q", out, tc.want)
			}
		})
	}
}

// A configured HTTPS_PROXY host dials unguarded even though it resolves into the loopback range
// (RFC1918/loopback proxies are common on a dev box); a base_url host NOT in proxyHosts resolving
// to the same range is still refused. proxyHosts is swapped directly since it is process-global and
// resolveProxyHosts itself is covered (env → map) by TestResolveProxyHosts above.
func TestGuardedDialProxyHostVsBaseURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	port := strings.TrimPrefix(srv.URL, "http://127.0.0.1:")

	old := proxyHosts
	proxyHosts = map[string]bool{"127.0.0.1": true}
	t.Cleanup(func() { proxyHosts = old })

	conn, err := guardedDial(context.Background(), "tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("configured proxy host must dial unguarded: %v", err)
	}
	_ = conn.Close()

	// Regression guard: a non-proxy host resolving to loopback is still refused, proxyHosts notwithstanding.
	proxyHosts = map[string]bool{"proxy.corp": true}
	_, err = guardedDialer.DialContext(context.Background(), "tcp", "127.0.0.1:"+port)
	if err == nil || !strings.Contains(err.Error(), "resolves into the local network") {
		t.Errorf("non-proxy host resolving to loopback must still be refused: %v", err)
	}
}
