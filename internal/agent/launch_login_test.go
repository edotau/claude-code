package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Seams the fake sign-in drives: the login and the credential check (the u2m sign-in's two calls).
var (
	testLoginRow  func(context.Context, string, io.Writer) error
	testCredCheck = providers.Credential
)

// check is captured at construction so an abandoned Check never reads testCredCheck while a cleanup restores it.
type fakeSignin struct {
	p     *providers.Provider
	check func(context.Context, *providers.Provider) (string, error)
}

func (f fakeSignin) Check(ctx context.Context) (providers.AuthVerdict, string) {
	_, err := f.check(ctx, f.p)
	return providers.Verdict(err), ""
}

func (f fakeSignin) Login(ctx context.Context, _ io.Reader, out io.Writer) error {
	return testLoginRow(ctx, f.p.Name, out)
}

type loginHarness struct {
	calls  *atomic.Int32 // loginRow invocations
	mints  *atomic.Int32 // token endpoint hits
	out    *bytes.Buffer
	server string
}

// loginSetup fakes the TTY, prompt input, token endpoint and loginRow seam.
func loginSetup(t *testing.T, tty bool, input string, loginErr error) loginHarness {
	t.Helper()
	vaulttest.UseKeyring(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	h := loginHarness{calls: &atomic.Int32{}, mints: &atomic.Int32{}, out: &bytes.Buffer{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mints.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "acc", "expires_in": 3600})
	}))
	t.Cleanup(srv.Close)
	h.server = srv.URL
	oldRow, oldTTY, oldIn, oldOut, oldFor, oldCheck := testLoginRow, canPrompt, loginIn, loginOut, signinFor, testCredCheck
	t.Cleanup(func() {
		testLoginRow, canPrompt, loginIn, loginOut, signinFor, testCredCheck = oldRow, oldTTY, oldIn, oldOut, oldFor, oldCheck
	})
	signinFor = func(p *providers.Provider) Signin { return fakeSignin{p, testCredCheck} }
	canPrompt = func() bool { return tty }
	loginIn, loginOut = strings.NewReader(input), h.out
	testLoginRow = func(_ context.Context, name string, _ io.Writer) error {
		h.calls.Add(1)
		if loginErr != nil {
			return loginErr
		}
		return providers.SealSecret(providers.AuthSecretName(&providers.Provider{Name: name, Provider: "databricks", Auth: providers.Auth{Type: providers.AuthOAuth}}), `{"refresh":"rt"}`) // oauth = one JSON value
	}
	return h
}

// rowSeq makes each row name unique per run, so -count>1 never reuses a token the providers package cached.
var rowSeq atomic.Int32

func u2mRow(name, url string) *providers.Provider {
	name = fmt.Sprintf("%s-%d", name, rowSeq.Add(1))
	return &providers.Provider{Name: name, Provider: "databricks", Protocol: providers.ProtocolAnthropic, BaseURL: url + "/serving-endpoints/anthropic",
		Auth: providers.Auth{Type: providers.AuthOAuth, TTL: 1800}}
}

func TestOfferLoginSuccessRetries(t *testing.T) {
	h := loginSetup(t, true, "\n", nil)
	var plan Plan
	offerLogin(context.Background(), LaunchOptions{}, nil, []*providers.Provider{u2mRow("lp-ok", h.server)}, &plan)
	if h.calls.Load() != 1 || h.mints.Load() != 1 || len(plan.Warnings) != 0 {
		t.Fatalf("login=%d mints=%d warnings=%v", h.calls.Load(), h.mints.Load(), plan.Warnings)
	}
	if !strings.Contains(h.out.String(), "Log in now? [Y/n]") {
		t.Errorf("prompt = %q", h.out.String())
	}
}

func TestOfferLoginFailureWarns(t *testing.T) {
	h := loginSetup(t, true, "y\n", errors.New("browser closed"))
	var plan Plan
	offerLogin(context.Background(), LaunchOptions{}, nil, []*providers.Provider{u2mRow("lp-fail", h.server)}, &plan)
	if h.calls.Load() != 1 || len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "browser closed") {
		t.Fatalf("login=%d warnings=%v", h.calls.Load(), plan.Warnings)
	}
}

func TestOfferLoginSkippedWhenNotInteractive(t *testing.T) {
	cases := map[string]struct {
		tty  bool
		o    LaunchOptions
		args []string
	}{
		"non-tty":   {false, LaunchOptions{}, nil},
		"print-env": {true, LaunchOptions{PrintEnv: true}, nil},
		"headless":  {true, LaunchOptions{}, []string{"-p", "hi"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := loginSetup(t, c.tty, "y\n", nil)
			var plan Plan
			offerLogin(context.Background(), c.o, c.args, []*providers.Provider{u2mRow("lp-skip", h.server)}, &plan)
			if h.calls.Load() != 0 || h.out.Len() != 0 || len(plan.Warnings) != 0 {
				t.Fatalf("login=%d out=%q warnings=%v", h.calls.Load(), h.out.String(), plan.Warnings)
			}
		})
	}
}

func TestOfferLoginDecliningWarns(t *testing.T) {
	h := loginSetup(t, true, "n\n", nil)
	var plan Plan
	offerLogin(context.Background(), LaunchOptions{}, nil, []*providers.Provider{u2mRow("lp-no", h.server)}, &plan)
	if h.calls.Load() != 0 || len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "login skipped") {
		t.Fatalf("login=%d warnings=%v", h.calls.Load(), plan.Warnings)
	}
	if !strings.Contains(h.out.String(), "Log in now?") {
		t.Errorf("expected a prompt, got %q", h.out.String())
	}
}

func TestOfferLoginSkipsNilAndDedupes(t *testing.T) {
	h := loginSetup(t, true, "y\n", errors.New("x"))
	var plan Plan
	dup := u2mRow("lp-dup", h.server)
	offerLogin(context.Background(), LaunchOptions{}, nil, []*providers.Provider{nil, dup, dup}, &plan)
	if h.calls.Load() != 1 {
		t.Errorf("duplicate provider logged in %d times, want 1", h.calls.Load())
	}
}

func TestOfferLoginSharesOneReaderAcrossProviders(t *testing.T) {
	h := loginSetup(t, true, "\nn\n", nil)
	var plan Plan
	ps := []*providers.Provider{u2mRow("lp-a", h.server), u2mRow("lp-b", h.server)}
	offerLogin(context.Background(), LaunchOptions{}, nil, ps, &plan)
	if h.calls.Load() != 1 || len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "lp-b") {
		t.Fatalf("login=%d warnings=%v", h.calls.Load(), plan.Warnings)
	}
}

// A hung token check gives up after credCheckTimeout: warn, never prompt, never log in, launch anyway.
func TestOfferLoginSlowTokenCheckLaunchesWithoutPrompt(t *testing.T) {
	h := loginSetup(t, true, "y\n", nil)
	release := make(chan struct{})
	oldCheck, oldTimeout := testCredCheck, credCheckTimeout
	t.Cleanup(func() { close(release); testCredCheck, credCheckTimeout = oldCheck, oldTimeout })
	testCredCheck = func(context.Context, *providers.Provider) (string, error) {
		<-release
		return "", providers.ErrNeedsLogin
	}
	credCheckTimeout = 50 * time.Millisecond
	var plan Plan
	start := time.Now()
	offerLogin(context.Background(), LaunchOptions{}, nil, []*providers.Provider{u2mRow("slow", h.server)}, &plan)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("offerLogin took %s, want it bounded by credCheckTimeout", d)
	}
	if h.calls.Load() != 0 || strings.Contains(h.out.String(), "Log in now?") {
		t.Fatalf("prompted or logged in on a timed-out check (logins=%d, out=%q)", h.calls.Load(), h.out.String())
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "launching anyway") {
		t.Fatalf("warnings = %q, want one 'launching anyway' warning", plan.Warnings)
	}
}

// A real token endpoint that hangs: the bounded check warns once, never prompts, never logs in.
func TestOfferLoginHungTokenCheckLaunchesWithoutPrompt(t *testing.T) {
	h := loginSetup(t, true, "y\n", nil)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs before srv.Close so the abandoned mint returns promptly
	p := u2mRow("hung", srv.URL)
	if err := providers.SealSecret(providers.AuthSecretName(p), `{"refresh":"rt"}`); err != nil {
		t.Fatal(err)
	}
	oldTimeout := credCheckTimeout
	t.Cleanup(func() { credCheckTimeout = oldTimeout })
	credCheckTimeout = 100 * time.Millisecond
	var plan Plan
	start := time.Now()
	offerLogin(context.Background(), LaunchOptions{}, nil, []*providers.Provider{p}, &plan)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("offerLogin took %s, want it bounded by credCheckTimeout", d)
	}
	if h.calls.Load() != 0 || strings.Contains(h.out.String(), "Log in now?") {
		t.Fatalf("prompted or logged in (logins=%d, out=%q)", h.calls.Load(), h.out.String())
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "timed out") {
		t.Fatalf("warnings = %q, want exactly one timeout warning", plan.Warnings)
	}
}

// The check after a successful login is bounded too: a hung re-check warns instead of stalling the launch.
func TestOfferLoginHungPostLoginCheckWarns(t *testing.T) {
	h := loginSetup(t, true, "y\n", nil)
	release := make(chan struct{})
	var calls atomic.Int32
	oldCheck, oldTimeout := testCredCheck, credCheckTimeout
	t.Cleanup(func() { close(release); testCredCheck, credCheckTimeout = oldCheck, oldTimeout })
	testCredCheck = func(context.Context, *providers.Provider) (string, error) {
		if calls.Add(1) == 1 {
			return "", providers.ErrNeedsLogin
		}
		<-release
		return "tok", nil
	}
	credCheckTimeout = 50 * time.Millisecond
	var plan Plan
	start := time.Now()
	offerLogin(context.Background(), LaunchOptions{}, nil, []*providers.Provider{u2mRow("post", h.server)}, &plan)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("offerLogin took %s, want the post-login check bounded by credCheckTimeout", d)
	}
	if h.calls.Load() != 1 || len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "timed out") {
		t.Fatalf("logins=%d warnings=%q, want one login and one timeout warning", h.calls.Load(), plan.Warnings)
	}
}

// verdictSignin drives offerLogin with a fixed verdict for any kind (not only u2m).
type verdictSignin struct {
	v      providers.AuthVerdict
	logins *atomic.Int32
}

func (s verdictSignin) Check(context.Context) (providers.AuthVerdict, string) { return s.v, "" }
func (s verdictSignin) Login(context.Context, io.Reader, io.Writer) error {
	s.logins.Add(1)
	return nil
}

func TestOfferLoginUsesSigninForAllKinds(t *testing.T) {
	kinds := map[string]providers.Auth{
		"subscription": {Type: providers.AuthCLI},
		"codex":        {Type: providers.AuthCLI, Login: "codex"},
		"pat":          {Type: providers.AuthPAT},
		"env-key":      {Type: providers.AuthKey, Env: "X_KEY"},
		"u2m":          {Type: providers.AuthOAuth},
	}
	for kind, auth := range kinds {
		for v, wantLogin := range map[providers.AuthVerdict]bool{
			providers.VerdictNeedsLogin: true, providers.VerdictRejected: true,
			providers.VerdictTransient: false, providers.VerdictOK: false,
		} {
			t.Run(kind+"/"+string(v), func(t *testing.T) {
				h := loginSetup(t, true, "y\n", nil)
				var n atomic.Int32
				signinFor = func(*providers.Provider) Signin { return verdictSignin{v: v, logins: &n} }
				var plan Plan
				offerLogin(context.Background(), LaunchOptions{}, nil, []*providers.Provider{{Name: "r-" + kind, Auth: auth}}, &plan)
				if kind == "subscription" { // Claude Code shows its own login screen
					wantLogin = false
				}
				if (n.Load() == 1) != wantLogin || n.Load() > 1 {
					t.Fatalf("logins=%d want login=%v (out %q)", n.Load(), wantLogin, h.out.String())
				}
			})
		}
	}
}

// A passthrough row with no Auth.Login (the default subscription) is never checked: Claude Code has its own login.
func TestOfferLoginSubscriptionRunsNoCheck(t *testing.T) {
	for name, tc := range map[string]struct {
		auth      providers.Auth
		wantCheck bool
	}{
		"subscription": {providers.Auth{Type: providers.AuthCLI}, false},
		"codex":        {providers.Auth{Type: providers.AuthCLI, Login: "codex"}, true},
		"pat":          {providers.Auth{Type: providers.AuthPAT}, true},
	} {
		t.Run(name, func(t *testing.T) {
			loginSetup(t, true, "y\n", nil)
			var checks atomic.Int32
			signinFor = func(*providers.Provider) Signin { return countSignin{&checks} }
			offerLogin(context.Background(), LaunchOptions{}, nil, []*providers.Provider{{Name: "r-" + name, Auth: tc.auth}}, &Plan{})
			if (checks.Load() > 0) != tc.wantCheck {
				t.Fatalf("checks=%d, want check=%v", checks.Load(), tc.wantCheck)
			}
		})
	}
}

type countSignin struct{ n *atomic.Int32 }

func (c countSignin) Check(context.Context) (providers.AuthVerdict, string) {
	c.n.Add(1)
	return providers.VerdictOK, ""
}
func (countSignin) Login(context.Context, io.Reader, io.Writer) error { return nil }

func TestOfferLoginSkippedHintNamesConfigureProviderAll(t *testing.T) {
	loginSetup(t, true, "n\n", nil)
	signinFor = func(*providers.Provider) Signin {
		return verdictSignin{v: providers.VerdictNeedsLogin, logins: &atomic.Int32{}}
	}
	var plan Plan
	offerLogin(context.Background(), LaunchOptions{}, nil, []*providers.Provider{{Name: "qa-row", Auth: providers.Auth{Type: providers.AuthPAT}}}, &plan)
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "configure provider --all qa-row") {
		t.Fatalf("warnings = %v", plan.Warnings)
	}
}

// barrierSignin's first Check blocks until every sibling's first Check has started (or 3s pass): only concurrent
// checks reach the barrier together. Later Checks (the post-login recheck) return ok.
type barrierSignin struct {
	name    string
	arrived *atomic.Int32
	want    int32
	met     *atomic.Int32
	first   *atomic.Bool
	order   *[]string
	mu      *sync.Mutex
}

func (b barrierSignin) Check(context.Context) (providers.AuthVerdict, string) {
	if !b.first.CompareAndSwap(false, true) {
		return providers.VerdictOK, ""
	}
	b.arrived.Add(1)
	deadline := time.Now().Add(3 * time.Second)
	for b.arrived.Load() < b.want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if b.arrived.Load() >= b.want {
		b.met.Add(1)
	}
	return providers.VerdictNeedsLogin, ""
}

func (b barrierSignin) Login(context.Context, io.Reader, io.Writer) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	*b.order = append(*b.order, b.name)
	return nil
}

func TestOfferLoginChecksConcurrentlyPromptsInOrder(t *testing.T) {
	h := loginSetup(t, true, "y\ny\ny\n", nil)
	names := []string{"c-a", "c-b", "c-c"}
	var arrived atomic.Int32
	var met atomic.Int32
	var mu sync.Mutex
	var order []string
	stubs := map[string]barrierSignin{}
	for _, n := range names {
		stubs[n] = barrierSignin{n, &arrived, int32(len(names)), &met, &atomic.Bool{}, &order, &mu}
	}
	signinFor = func(p *providers.Provider) Signin { return stubs[p.Name] }
	var ps []*providers.Provider
	for _, n := range names {
		ps = append(ps, &providers.Provider{Name: n, Auth: providers.Auth{Type: providers.AuthPAT}})
	}
	ps = append(ps, ps[0], nil) // duplicate and nil are skipped
	var plan Plan
	offerLogin(context.Background(), LaunchOptions{}, nil, ps, &plan)
	if met.Load() != int32(len(names)) {
		t.Fatalf("checks did not run concurrently: only %d of %d saw their siblings running", met.Load(), len(names))
	}
	if strings.Join(order, ",") != "c-a,c-b,c-c" {
		t.Errorf("logins ran in order %v, want original order", order)
	}
	if got := strings.Count(h.out.String(), "Log in now?"); got != 3 {
		t.Errorf("prompts = %d, want 3 (dedupe): %q", got, h.out.String())
	}
	if a, b := strings.Index(h.out.String(), "c-a:"), strings.Index(h.out.String(), "c-b:"); a > b {
		t.Errorf("prompts out of order: %q", h.out.String())
	}
}
