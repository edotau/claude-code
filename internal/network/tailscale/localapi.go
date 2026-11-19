package tailscale

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

// errNoLocalAPI: no tailscaled LocalAPI endpoint accepted a connection; callers fall back to the CLI.
var errNoLocalAPI = errors.New("tailscale: no LocalAPI endpoint reachable")

// localAPIHost is the Host tailscaled's DNS-rebinding guard requires; nothing resolves it.
const localAPIHost = "local-tailscaled.sock"

// maxLocalAPIBody caps one LocalAPI response read.
const maxLocalAPIBody = 16 << 20

// endpoint is one way to reach tailscaled: a unix socket, or a loopback TCP port with a basic-auth token.
type endpoint struct{ network, addr, token string }

// Discovery seams in Tailscale's own order (safesocket_darwin.go, paths.go); off under go test unless stubbed.
var (
	localAPIOff = testing.Testing()
	macsysDir   = "/Library/Tailscale"
	unixSockets = func() []string {
		return []string{"/var/run/tailscale/tailscaled.sock", "/var/run/tailscaled.socket", harnessSocket()}
	}
)

// harnessSocket is where `install --tools` points a rootless Linux tailscaled (tools/install.go:596).
func harnessSocket() string { return filepath.Join(paths.StateDir(), "tailscale", "tailscaled.sock") }

// StubLocalAPI points discovery at sock alone ("" = no LocalAPI), for tests; restore undoes it.
func StubLocalAPI(sock string) (restore func()) {
	off, dir, socks := localAPIOff, macsysDir, unixSockets
	localAPIOff, macsysDir = sock == "", ""
	unixSockets = func() []string { return []string{sock} }
	return func() { localAPIOff, macsysDir, unixSockets = off, dir, socks }
}

// endpoints lists candidates: the macOS standalone app's TCP port first, then every existing unix socket.
func endpoints() []endpoint {
	if localAPIOff {
		return nil
	}
	var out []endpoint
	if goos == "darwin" && macsysDir != "" {
		if ep, ok := macsysEndpoint(macsysDir); ok {
			out = append(out, ep)
		}
	}
	for _, s := range unixSockets() {
		if _, err := os.Stat(s); err == nil {
			out = append(out, endpoint{network: "unix", addr: s})
		}
	}
	return out
}

// macsysEndpoint reads the standalone app's port (ipnport symlink) and token (sameuserproof-<port>, 0640 root:admin).
func macsysEndpoint(dir string) (endpoint, bool) {
	port, err := os.Readlink(filepath.Join(dir, "ipnport"))
	if err != nil {
		return endpoint{}, false
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return endpoint{}, false
	}
	tok, err := os.ReadFile(filepath.Join(dir, "sameuserproof-"+port))
	if err != nil {
		return endpoint{}, false
	}
	return endpoint{network: "tcp", addr: net.JoinHostPort("127.0.0.1", port), token: strings.TrimSpace(string(tok))}, true
}

// isDial reports a connect failure, the one error that moves on to the next endpoint.
func isDial(err error) bool {
	oe, ok := errors.AsType[*net.OpError](err)
	return ok && oe.Op == "dial"
}

// localAPI sends method /localapi/v0/<path> to the first endpoint that accepts; errNoLocalAPI when none does.
func localAPI(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultExecTimeout)
	defer cancel()
	for _, ep := range endpoints() {
		out, err := ep.do(ctx, method, path, body)
		if isDial(err) {
			continue
		}
		return out, err
	}
	return nil, errNoLocalAPI
}

// do runs one request on a fresh connection to ep (no proxy); a non-2xx answer carries tailscaled's text.
func (ep endpoint) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	tr := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, ep.network, ep.addr)
	}}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, method, "http://"+localAPIHost+"/localapi/v0/"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if ep.token != "" {
		req.SetBasicAuth("", ep.token)
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("tailscale localapi %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxLocalAPIBody))
	if err != nil {
		return nil, fmt.Errorf("tailscale localapi %s: %w", path, err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("tailscale localapi %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}
