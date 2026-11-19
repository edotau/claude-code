package tailscale

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

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

// prep builds /localapi/v0/<path> with ep's token, and a client on a fresh connection to ep (no proxy).
func (ep endpoint) prep(ctx context.Context, method, path string, body io.Reader) (*http.Client, *http.Request, error) {
	tr := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, ep.network, ep.addr)
	}}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+localAPIHost+"/localapi/v0/"+path, body)
	if err == nil && ep.token != "" {
		req.SetBasicAuth("", ep.token)
	}
	return &http.Client{Transport: tr}, req, err
}

// do runs one request on a fresh connection to ep (no proxy); a non-2xx answer carries tailscaled's text.
func (ep endpoint) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	c, req, err := ep.prep(ctx, method, path, bytes.NewReader(body))
	defer c.CloseIdleConnections()
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
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

// prefsMask is the PATCH /localapi/v0/prefs body (ipn.MaskedPrefs): a field applies only with its <Field>Set.
type prefsMask struct {
	AdvertiseRoutes           []string
	AdvertiseRoutesSet        bool   `json:",omitempty"`
	ExitNodeIP                string `json:",omitempty"`
	ExitNodeIPSet             bool   `json:",omitempty"`
	ExitNodeID                string `json:",omitempty"`
	ExitNodeIDSet             bool   `json:",omitempty"`
	ExitNodeAllowLANAccess    bool   `json:",omitempty"`
	ExitNodeAllowLANAccessSet bool   `json:",omitempty"`
	WantRunning               bool   `json:",omitempty"`
	WantRunningSet            bool   `json:",omitempty"`
	LoggedOut                 bool   `json:",omitempty"`
	LoggedOutSet              bool   `json:",omitempty"`
}

// patchPrefs applies m over the LocalAPI.
func patchPrefs(ctx context.Context, m prefsMask) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = localAPI(ctx, http.MethodPatch, "prefs", body)
	return err
}

// currentRoutes is the AdvertiseRoutes pref: subnets, plus both default routes while an exit node.
func currentRoutes(ctx context.Context) ([]string, error) {
	out, err := localAPI(ctx, http.MethodGet, "prefs", nil)
	if err != nil {
		return nil, err
	}
	var p struct{ AdvertiseRoutes []string }
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, fmt.Errorf("tailscale localapi prefs: %w", err)
	}
	return p.AdvertiseRoutes, nil
}

// Values from ipn/backend.go (checked in Phase 5.0).
const (
	ipnRunning         = 6
	notifyInitialState = 2
)

// maxNotify caps one watch-ipn-bus message (a JSON line); 1.104 notifies carry no netmap.
const maxNotify = 1 << 20

// signInWait bounds a watch that is neither Running nor showing a sign-in URL; a shown URL gets the full timeout.
const signInWait = 30 * time.Second

// errNoSignIn: tailscaled neither reached Running nor offered a sign-in URL within signInWait.
var errNoSignIn = errors.New("tailscale: not Running and no sign-in URL after 30s")

// errKeyNotAccepted: a connect (no one to open a browser) was offered a sign-in URL, so the auth key did not apply.
var errKeyNotAccepted = errors.New("tailscale: the auth key was not accepted (tailscaled asked for a browser sign-in)")

// notify is the subset of ipn.Notify a login watch reads.
type notify struct {
	State       *int
	BrowseToURL *string
	ErrMessage  *string
}

// localAPIStream opens a streaming GET (watch-ipn-bus) bounded only by ctx; the caller closes it.
func localAPIStream(ctx context.Context, path string) (io.ReadCloser, error) {
	for _, ep := range endpoints() {
		c, req, err := ep.prep(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.Do(req)
		if isDial(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("tailscale localapi %s: %w", path, err)
		}
		if resp.StatusCode/100 != 2 {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("tailscale localapi %s: HTTP %d", path, resp.StatusCode)
		}
		return resp.Body, nil
	}
	return nil, errNoLocalAPI
}

// loginLocal watches the IPN bus, runs start, prints the sign-in URL once and waits for Running.
func loginLocal(ctx context.Context, start func(context.Context) error, out io.Writer) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	bus, err := localAPIStream(ctx, "watch-ipn-bus?mask="+strconv.Itoa(notifyInitialState))
	if err != nil {
		return err
	}
	defer func() { _ = bus.Close() }()
	if err := start(ctx); err != nil {
		return err
	}
	wait := time.AfterFunc(signInWait, func() { cancel(errNoSignIn) })
	defer wait.Stop()
	if err := awaitRunning(bus, out, func() { wait.Stop() }); err != nil {
		if cause := context.Cause(ctx); errors.Is(cause, errNoSignIn) {
			return cause
		}
		return err
	}
	return nil
}

// awaitRunning reads notifies until Running or an error; the first fresh sign-in URL goes to out, then onURL runs.
func awaitRunning(bus io.Reader, out io.Writer, onURL func()) error {
	sc := bufio.NewScanner(bus)
	sc.Buffer(nil, maxNotify)
	for first, shown := true, false; sc.Scan(); first = false {
		var n notify
		if err := json.Unmarshal(sc.Bytes(), &n); err != nil {
			return fmt.Errorf("tailscale localapi watch-ipn-bus: %w", err)
		}
		if done, err := n.outcome(); done || err != nil {
			return err
		}
		u, ok := signInURL(n.BrowseToURL)
		switch {
		case !ok || first || shown: // the initial notify may hold a stale URL from an earlier login
		case out == nil:
			return errKeyNotAccepted
		default:
			fmt.Fprintf(out, "\nTo authenticate, visit:\n\n\t%s\n\n", u)
			shown = true
			onURL()
		}
	}
	return fmt.Errorf("tailscale localapi watch-ipn-bus: %w", cmp.Or(sc.Err(), io.ErrUnexpectedEOF))
}

// outcome ends the watch: done at Running, or tailscaled's error message.
func (n notify) outcome() (done bool, err error) {
	if n.ErrMessage != nil && *n.ErrMessage != "" {
		return true, fmt.Errorf("tailscale: %s", printable(*n.ErrMessage))
	}
	return n.State != nil && *n.State == ipnRunning, nil
}

// signInURL is a notify's BrowseToURL when it is an https URL, control characters stripped.
func signInURL(s *string) (string, bool) {
	if s == nil {
		return "", false
	}
	u, err := url.Parse(printable(*s))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", false
	}
	return u.String(), true
}

// printable drops control and format runes, so daemon text cannot drive the terminal.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}

// startWithKey masks WantRunning (and exit routes) on, starts with only the AuthKey, then logs in when keyless.
func startWithKey(ctx context.Context, authKey string, exitNode bool) error {
	m := prefsMask{WantRunning: true, WantRunningSet: true, LoggedOutSet: true}
	if exitNode {
		cur, err := currentRoutes(ctx)
		if err != nil {
			return err
		}
		m.AdvertiseRoutes, m.AdvertiseRoutesSet = append(subnetRoutes(cur), defaultRoutes...), true
	}
	hadKey, err := haveNodeKey(ctx)
	if err != nil {
		return err
	}
	if err := patchPrefs(ctx, m); err != nil {
		return err
	}
	body, err := json.Marshal(struct{ AuthKey string }{authKey}) // no UpdatePrefs: Start keeps current prefs
	if err != nil {
		return err
	}
	if _, err = localAPI(ctx, http.MethodPost, "start", body); err != nil || (hadKey && authKey != "") {
		return err
	}
	_, err = localAPI(ctx, http.MethodPost, "login-interactive", nil) // as `tailscale up`: start alone never logs in
	return err
}

// haveNodeKey reports status's HaveNodeKey, read before start as `tailscale up` does.
func haveNodeKey(ctx context.Context) (bool, error) {
	out, err := localAPI(ctx, http.MethodGet, "status?peers=false", nil)
	if err != nil {
		return false, err
	}
	var st struct{ HaveNodeKey bool }
	if err := json.Unmarshal(out, &st); err != nil {
		return false, fmt.Errorf("tailscale localapi status: %w", err)
	}
	return st.HaveNodeKey, nil
}
