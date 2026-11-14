package databricks

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

const remoteHost = "wks.example.ts.net"

// remoteWorld is a Databricks profile with a paired service principal on a fake token endpoint, plus fakes for the
// two ssh calls and this machine's opener; the fake host browser follows the authorization URL back through the "forward".
type remoteWorld struct {
	codes, mints, opensLocal atomic.Int32
	forwards                 [][4]any // host, first, last, local of each ReverseForward
	stops                    atomic.Int32
	hostPort                 func(local int) int // the host port the fake forward lands on
	forwardErr               error
	opened                   []string // URLs the host was asked to open
	redirect                 string   // redirect_uri of the URL the host browser got
	localPort                int
}

func newRemoteWorld(t *testing.T) *remoteWorld {
	t.Helper()
	w := &remoteWorld{hostPort: func(local int) int { return local }}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		rw.Header().Set("Content-Type", "application/json")
		if r.PostForm.Get("grant_type") == "client_credentials" {
			w.mints.Add(1)
			fmt.Fprint(rw, `{"access_token":"sp-access-0001","expires_in":3600}`)
			return
		}
		w.codes.Add(1)
		if r.PostForm.Get("redirect_uri") != w.redirect {
			http.Error(rw, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		fmt.Fprint(rw, `{"access_token":"user-access-0001","refresh_token":"user-refresh-0001","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)
	seedOverlay(t, fmt.Sprintf(`{"providers":{"databricks":{"dbx-rem":{"protocol":"anthropic","base_url":%q,`+
		`"auth":{"type":"oauth","spClientId":"sp-client"},"identity":"auto"}}}}`, srv.URL))
	if err := vault.Store("DATABRICKS_DBX_REM_M2M", "sp-secret-value"); err != nil {
		t.Fatal(err)
	}
	stubValid(t, false)
	t.Setenv(remote.LoginRemoteEnv, remoteHost)
	w.swapSeams(t)
	return w
}

func (w *remoteWorld) swapSeams(t *testing.T) {
	t.Helper()
	oldLocal, oldFwd, oldOpen, oldWait := openLocal, forwardPort, openOnHost, loginDeadline
	t.Cleanup(func() { openLocal, forwardPort, openOnHost, loginDeadline = oldLocal, oldFwd, oldOpen, oldWait })
	loginDeadline = 3 * time.Second // a login that never gets its callback fails the test instead of hanging it
	openLocal = func(string) error { w.opensLocal.Add(1); return errors.New("a browser was opened on this machine") }
	forwardPort = func(_ context.Context, host string, first, last, local int) (int, func(), error) {
		w.forwards = append(w.forwards, [4]any{host, first, last, local})
		if w.forwardErr != nil {
			return 0, nil, w.forwardErr
		}
		w.localPort = local
		return w.hostPort(local), func() { w.stops.Add(1) }, nil
	}
	openOnHost = func(_ context.Context, host, authURL string) error {
		w.opened = append(w.opened, host+" "+authURL)
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		q := u.Query()
		w.redirect = q.Get("redirect_uri")
		// The host's browser lands on localhost:<redirect port>, which the forward carries to this machine's listener.
		cb := "http://127.0.0.1:" + strconv.Itoa(w.localPort) + "/?code=the-code&state=" + url.QueryEscape(q.Get("state"))
		resp, err := http.Get(cb)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}
}

func (w *remoteWorld) sealed(t *testing.T) bool {
	t.Helper()
	return providers.HasSecret("DATABRICKS_DBX_REM_OAUTH") != ""
}

func TestRemoteLoginSignsInOnHostAndSealsHere(t *testing.T) {
	for _, c := range []struct {
		name     string
		hostPort func(int) int
	}{
		{"same port", func(l int) int { return l }},
		{"port taken on the host, next one used", func(l int) int { return l + 1 }},
	} {
		w := newRemoteWorld(t)
		w.hostPort = c.hostPort
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return login([]string{"-p", "dbx-rem"}) })
		if code != 0 || w.codes.Load() != 1 || w.mints.Load() != 0 || !w.sealed(t) || w.opensLocal.Load() != 0 {
			t.Fatalf("%s: exit %d codes %d mints %d sealed %v local opens %d stderr %q; want a user sign-in, no mint, sealed here, no local browser",
				c.name, code, w.codes.Load(), w.mints.Load(), w.sealed(t), w.opensLocal.Load(), stderr)
		}
		if len(w.forwards) != 1 || w.forwards[0][0] != remoteHost || w.forwards[0][2] != 8050 || w.stops.Load() != 1 {
			t.Errorf("%s: forwards %v stops %d; want one forward on %s up to 8050, closed", c.name, w.forwards, w.stops.Load(), remoteHost)
		}
		if want := "http://localhost:" + strconv.Itoa(w.hostPort(w.localPort)); w.redirect != want {
			t.Errorf("%s: redirect_uri %q, want the host's port %q", c.name, w.redirect, want)
		}
		if len(w.opened) != 1 || !strings.HasPrefix(w.opened[0], remoteHost+" http") || !strings.Contains(stderr, "/oidc/v1/authorize?") {
			t.Errorf("%s: opened %q, stderr %q; want the host asked once and the URL printed here", c.name, w.opened, stderr)
		}
		if w.opensLocal.Load() != 0 {
			t.Errorf("%s: a browser was opened on this machine", c.name)
		}
	}
}

func TestRemoteLoginExactPortTakenNamesPortAndHost(t *testing.T) {
	w := newRemoteWorld(t)
	w.forwardErr = &remote.PortTakenError{Host: remoteHost, First: 8040, Last: 8040}
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return login([]string{"-p", "dbx-rem", "--port", "8040"}) })
	if code != 1 || !strings.Contains(stderr, "port 8040 on "+remoteHost) || len(w.opened) != 0 || w.sealed(t) {
		t.Errorf("exit %d stderr %q opened %q", code, stderr, w.opened)
	}
	if f := w.forwards[0]; f[1] != 8040 || f[2] != 8040 {
		t.Errorf("forward range %v, want exactly 8040", f)
	}
}

func TestRemoteLoginSSHFailureNamesHostAndPasteAndOpensNoLocalBrowser(t *testing.T) {
	w := newRemoteWorld(t)
	w.forwardErr = errors.New("ssh to " + remoteHost + ": Connection refused")
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return login([]string{"-p", "dbx-rem"}) })
	if code != 1 || !strings.Contains(stderr, remoteHost) || !strings.Contains(stderr, "--paste") || strings.Count(strings.TrimSpace(stderr), "\n") != 0 {
		t.Errorf("exit %d stderr %q, want one line naming %s and --paste", code, stderr, remoteHost)
	}
	if w.opensLocal.Load() != 0 || len(w.opened) != 0 || w.sealed(t) {
		t.Errorf("local opens %d, host opens %q, sealed %v; want none", w.opensLocal.Load(), w.opened, w.sealed(t))
	}
}

// With --paste a forward that cannot be made is not an error: the URL is printed and the redirected URL can be pasted.
func TestRemoteForwardFailureWithPasteContinuesWithoutOpening(t *testing.T) {
	w := newRemoteWorld(t)
	w.forwardErr = errors.New("ssh to " + remoteHost + ": Connection refused")
	opts := headless{paste: true, via: remoteHost}.remoteOptions()
	var buf strings.Builder
	opts.Out = &buf
	port, stop, err := opts.Forward(context.Background(), 8031)
	if err != nil || port != 8031 || stop == nil {
		t.Fatalf("port %d err %v, want the local port and no error", port, err)
	}
	if err := opts.Open("https://x/oidc/v1/authorize"); err == nil || len(w.opened) != 0 || w.opensLocal.Load() != 0 {
		t.Errorf("open err %v, host opens %q, local opens %d; want a refusal and no browser anywhere", err, w.opened, w.opensLocal.Load())
	}
	if opts.Paste == nil {
		t.Error("--paste not carried into the options")
	}
}

func TestRemoteLoginAsSPMintsWithNoForward(t *testing.T) {
	w := newRemoteWorld(t)
	code, stdout, _ := hosttest.CaptureOutput(t, func() int { return login([]string{"-p", "dbx-rem", "--as", "sp"}) })
	if code != 0 || w.mints.Load() != 1 || len(w.forwards) != 0 || len(w.opened) != 0 || w.opensLocal.Load() != 0 ||
		!strings.Contains(stdout, "service principal sp-client · token minted") {
		t.Errorf("exit %d mints %d forwards %v opened %q stdout %q", code, w.mints.Load(), w.forwards, w.opened, stdout)
	}
}

// Without --remote a profile with a paired service principal still mints and opens no browser.
func TestBareLoginWithPairedSPIsUnchanged(t *testing.T) {
	w := newRemoteWorld(t)
	t.Setenv(remote.LoginRemoteEnv, "")
	code, stdout, _ := hosttest.CaptureOutput(t, func() int { return login([]string{"-p", "dbx-rem"}) })
	if code != 0 || w.mints.Load() != 1 || w.codes.Load() != 0 || len(w.forwards) != 0 || w.opensLocal.Load() != 0 ||
		!strings.Contains(stdout, "service principal sp-client · token minted") {
		t.Errorf("exit %d mints %d codes %d forwards %v local opens %d stdout %q",
			code, w.mints.Load(), w.codes.Load(), w.forwards, w.opensLocal.Load(), stdout)
	}
}

func TestRemoteLoginRefusesNoBrowser(t *testing.T) {
	w := newRemoteWorld(t)
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return login([]string{"-p", "dbx-rem", "--no-browser"}) })
	if code != 2 || !strings.Contains(stderr, "--no-browser contradicts --remote") || len(w.forwards) != 0 {
		t.Errorf("exit %d stderr %q forwards %v", code, stderr, w.forwards)
	}
}

func TestRemoteLoginDryRunSaysWhatWouldHappenAndConnectsNowhere(t *testing.T) {
	w := newRemoteWorld(t)
	code, stdout, _ := hosttest.CaptureOutput(t, func() int { return login([]string{"-p", "dbx-rem", "--dry-run"}) })
	for _, want := range []string{"sign the user in for dbx-rem", "browser on " + remoteHost,
		"callback forwarded from " + remoteHost + ":8030", "to this machine", "sealed in this machine's vault", "no ssh connection made"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if code != 0 || len(w.forwards) != 0 || len(w.opened) != 0 || w.opensLocal.Load() != 0 || w.sealed(t) {
		t.Errorf("exit %d forwards %v opened %q sealed %v; a dry run does nothing", code, w.forwards, w.opened, w.sealed(t))
	}
}

// A login the variable routed names the cause as its first line; an explicit --remote prints no such line.
func TestAutoRemoteLoginSaysWhyFirst(t *testing.T) {
	w := newRemoteWorld(t)
	t.Setenv(remote.LoginAutoEnv, "1")
	code, stdout, _ := hosttest.CaptureOutput(t, func() int { return login([]string{"-p", "dbx-rem", "--dry-run"}) })
	want := "dbx-rem: CLAUDE_REMOTE_HOST is " + remoteHost + ": signing in through its browser (as --remote " + remoteHost + ")"
	first, rest, _ := strings.Cut(stdout, "\n")
	if code != 0 || first != want || !strings.Contains(rest, "no ssh connection made") || len(w.forwards) != 0 {
		t.Errorf("exit %d forwards %v first line %q, want %q then the remote dry-run text:\n%s", code, w.forwards, first, want, stdout)
	}
	t.Setenv(remote.LoginAutoEnv, "")
	_, stdout, _ = hosttest.CaptureOutput(t, func() int { return login([]string{"-p", "dbx-rem", "--dry-run"}) })
	if strings.Contains(stdout, "CLAUDE_REMOTE_HOST") {
		t.Errorf("an explicit --remote printed the variable's line:\n%s", stdout)
	}
}

func TestOpenURLVerbOpensOneHTTPSURL(t *testing.T) {
	var got []string
	old := openLocal
	openLocal = func(u string) error { got = append(got, u); return nil }
	t.Cleanup(func() { openLocal = old })
	if code, _, stderr := hosttest.CaptureOutput(t, func() int { return openURLVerb([]string{"https://x.example/a?b=1&c=2"}) }); code != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	for _, bad := range [][]string{{}, {"http://x.example"}, {"file:///etc/passwd"}, {"-n"}, {"https://a", "https://b"}} {
		if code, _, _ := hosttest.CaptureOutput(t, func() int { return openURLVerb(bad) }); code != 2 {
			t.Errorf("open-url %q: exit %d, want 2", bad, code)
		}
	}
	if len(got) != 1 || got[0] != "https://x.example/a?b=1&c=2" {
		t.Errorf("opened %q, want the one good URL", got)
	}
}
