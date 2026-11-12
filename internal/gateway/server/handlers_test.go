package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestAuthHintByAuthType(t *testing.T) {
	cases := map[string]struct {
		p    providers.Provider
		want string
	}{
		"oauth": {
			providers.Provider{Name: "d", Auth: providers.Auth{Type: providers.AuthOAuth}},
			"claude-code databricks login --profile d",
		},
		"preset": {
			providers.Provider{Name: "d", Preset: providers.PresetDatabricks},
			"claude-code databricks login --profile d",
		},
		"dbx pat": {
			providers.Provider{
				Name:   "d",
				Preset: providers.PresetDatabricks,
				Auth:   providers.Auth{Type: providers.AuthPAT},
			},
			"--profile d --auth-type oauth",
		},
		"key": {
			providers.Provider{Name: "k", Auth: providers.Auth{Type: providers.AuthKey, Env: "K_KEY"}},
			"claude-code vault set K_KEY",
		},
		"command": {
			providers.Provider{Name: "c", Auth: providers.Auth{Type: providers.AuthKey, Command: "op read x"}},
			"auth.command",
		},
		"other": {
			providers.Provider{Name: "o", Auth: providers.Auth{Type: providers.AuthCLI}},
			"claude-code configure provider --list o",
		},
	}
	for name, c := range cases {
		if got := authHint(&c.p); !strings.Contains(got, c.want) {
			t.Errorf("%s: authHint = %q, want %q", name, got, c.want)
		}
	}
}

func TestPeekBodyLeavesWholeBodyReadable(t *testing.T) {
	resp := &http.Response{Body: io.NopCloser(strings.NewReader("0123456789"))}
	if got := peekBody(resp, 4); string(got) != "0123" {
		t.Fatalf("peek = %q, want 0123", got)
	}
	rest, _ := io.ReadAll(resp.Body)
	if string(rest) != "0123456789" {
		t.Errorf("body after peek = %q, want the whole body", rest)
	}
}

func TestExhaustedSharesSendErrorReply(t *testing.T) {
	p := &providers.Provider{Name: "up1", Preset: providers.PresetDatabricks}
	ladder := []providers.Target{{Provider: p, Model: "m"}}
	cases := map[string]struct {
		err    error
		status int
		body   string
	}{
		"auth": {
			&fallbackError{error: errors.New("denied"), auth: true},
			http.StatusUnauthorized,
			"proxy: up1: authentication failed; run `claude-code databricks login --profile up1`",
		},
		"other": {errors.New("boom"), http.StatusBadGateway, "proxy: up1: boom"},
	}
	for name, c := range cases {
		rec := httptest.NewRecorder()
		name2, model, n := exhausted(rec, ladder, c.err)
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), c.body) || name2 != "up1" || model != "m" ||
			n != 1 {
			t.Errorf("%s: status %d body %q tuple %s/%s/%d", name, rec.Code, rec.Body.String(), name2, model, n)
		}
	}
}

// bumpMtime sets an exact timestamp to exercise second-granularity registry comparisons.
func bumpMtime(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// Failed registry loads must not advance the cached timestamp or hide a later fix at the same timestamp.
func TestRegistryRetriesLoadAfterAFailureRatherThanCachingIt(t *testing.T) {
	vaulttest.Use(t)
	const good = `{"providers":{}}`
	vaulttest.Put(t, vault.Overlay, "providers", good)
	r := newRouter(secret)
	r.log = io.Discard

	reg1, err := r.registry()
	if err != nil {
		t.Fatalf("first registry() = %v", err)
	}
	stamp1 := r.regStamp
	badStamp := stamp1.Add(time.Second)

	// Corrupt the overlay and force the vault file's mtime forward so registry() sees a change.
	vaulttest.Put(t, vault.Overlay, "providers", "not json")
	bumpMtime(t, vault.Path(), badStamp)

	reg2, err := r.registry()
	if err != nil {
		t.Fatalf("second registry() = %v, want it to keep the previous registry on a bad overlay", err)
	}
	if reg2 != reg1 {
		t.Error("a failed Load must keep serving the previous *Registry, not swap in a different one")
	}
	if r.regStamp.Equal(badStamp) {
		t.Fatal("regStamp advanced on a failed Load; a later fix landing at this same mtime would never be retried")
	}

	// Restore the overlay at the failed load timestamp; the unchanged cached timestamp must force a retry.
	vaulttest.Put(t, vault.Overlay, "providers", good)
	bumpMtime(t, vault.Path(), badStamp)

	reg3, err := r.registry()
	if err != nil {
		t.Fatalf("third registry() = %v, want the retried Load to succeed", err)
	}
	if reg3 == reg1 {
		t.Error("registry() served the stale cached registry instead of retrying Load at the same mtime")
	}
}

func TestRegistryLoadRunsWithoutHoldingRouterMutex(t *testing.T) {
	vaulttest.Use(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	original := loadRegistry
	loadRegistry = func() (*providers.Registry, error) {
		calls.Add(1)
		entered <- struct{}{}
		<-release
		return &providers.Registry{}, nil
	}
	t.Cleanup(func() { loadRegistry = original })

	r := newRouter(secret)
	firstDone := make(chan error, 1)
	go func() {
		_, err := r.registry()
		firstDone <- err
	}()
	<-entered
	if !r.mu.TryLock() {
		close(release)
		t.Fatal("router mutex is held during registry load")
	}
	r.mu.Unlock()

	secondStarted := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		close(secondStarted)
		_, err := r.registry()
		secondDone <- err
	}()
	<-secondStarted
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first registry() = %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second registry() = %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("registry load calls = %d, want 1", got)
	}
}

// u2mRow is a sealed-auth row with no sealed refresh token: minting yields providers.ErrNeedsLogin.
func u2mRow(base string) string {
	return fmt.Sprintf(
		`{"protocol":"anthropic","base_url":%q,"auth":{"type":"oauth"},"models":{"opus":"m1-opus","haiku":"m1-haiku"}}`,
		base)
}

func TestCondemnedRefreshMovesOnToNextTarget(t *testing.T) {
	g := useGate(t)
	up1 := newUpstream(t, ok)
	up2 := newUpstream(t, ok)
	t.Setenv("UP2_KEY", "k2")
	srv := setupRouter(t,
		`{"default":"up1","fallback":["up2"],"providers":{"databricks":{"up1":`+u2mRow(up1.URL)+`},"anthropic":{"up2":`+
			anthropicProvider(up2.URL, "UP2_KEY", `{"opus":"m2-opus","haiku":"m2-haiku"}`)+`}}}`,
		nil)

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-haiku"}`, nil)
	if resp.StatusCode != 200 || up1.hits.Load() != 0 || up2.hits.Load() != 1 {
		t.Fatalf("status %d hits up1=%d up2=%d, want 200 served by up2 only", resp.StatusCode, up1.hits.Load(),
			up2.hits.Load())
	}
	if reason, ok := g.marked("up1"); !ok || !strings.Contains(reason, "needs_login") ||
		!strings.Contains(reason, "databricks login --profile up1") {
		t.Fatalf("up1 mark = %q, %v; want a needs_login mark with the login hint", reason, ok)
	}
	if _, ok := g.marked("up2"); ok {
		t.Fatal("the healthy fallback was marked")
	}
	// attempts() now skips the marked provider: the next request goes straight to up2.
	if resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-haiku"}`, nil); resp.StatusCode != 200 ||
		up2.hits.Load() != 2 {
		t.Fatalf("second status %d up2 hits %d", resp.StatusCode, up2.hits.Load())
	}
}

func TestTransientRefreshFailureMovesOnWithoutMarking(t *testing.T) {
	g := useGate(t)
	up2 := newUpstream(t, ok)
	t.Setenv("UP2_KEY", "k2")
	srv := setupRouter(t,
		`{"default":"up1","fallback":["up2"],"providers":{"anthropic":{"up1":{"protocol":"anthropic","base_url":"`+up2.URL+
			`","auth":{"type":"key","command":"exit 1"},"models":{"opus":"m1"}},"up2":`+
			anthropicProvider(up2.URL, "UP2_KEY", `{"opus":"m2-opus"}`)+`}}}`,
		nil)

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1"}`, nil)
	if resp.StatusCode != 200 || up2.hits.Load() != 1 {
		t.Fatalf("status %d up2 hits %d, want a plain failover", resp.StatusCode, up2.hits.Load())
	}
	if g.count() != 0 {
		t.Fatalf("a transient failure condemned an account: %d", g.count())
	}
}

func invalidTokenUp(t *testing.T) *upstream {
	return newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(400)
		io.WriteString(w, `{"message":"Invalid Token"}`)
	})
}

func TestInvalidToken400OnDatabricksRowIsAnAuthRejection(t *testing.T) {
	g := useGate(t)
	up := invalidTokenUp(t)
	vaulttest.UseKeyring(t)
	row := sealedPAT(t, up.URL)
	srv := setupRouter2(t, `{"default":"patrow","providers":{"databricks":{"patrow":`+row+`}}}`)

	resp := post(t, srv.URL+"/p/patrow/v1/messages", `{"model":"m1"}`, nil)
	b := body(t, resp)
	if resp.StatusCode != 401 || !strings.Contains(b, "authentication_error") {
		t.Fatalf("status %d body %s, want 401 authentication_error", resp.StatusCode, b)
	}
	if up.hits.Load() != 2 {
		t.Fatalf("upstream hits %d, want the request plus one replay after the re-mint", up.hits.Load())
	}
	if _, ok := g.marked("patrow"); !ok {
		t.Fatal("a persistent Invalid Token must mark the provider")
	}
}

func TestInvalidToken400OnOtherProviderIsRelayedAsIs(t *testing.T) {
	g := useGate(t)
	up := invalidTokenUp(t)
	t.Setenv("UP1_KEY", "k1")
	srv := setupRouter(t,
		`{"default":"up1","providers":{"anthropic":{"up1":`+anthropicProvider(up.URL, "UP1_KEY", `{"opus":"m1"}`)+`}}}`,
		nil)

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1"}`, nil)
	if resp.StatusCode != 400 || !strings.Contains(body(t, resp), "Invalid Token") {
		t.Fatalf("status %d, want the upstream 400 relayed", resp.StatusCode)
	}
	if up.hits.Load() != 1 || g.count() != 0 {
		t.Fatalf("hits %d marks %d: a non-Databricks 400 must not remint or condemn", up.hits.Load(), g.count())
	}
}

// A bare 403 that survives the re-mint is a policy/WAF block: relayed to the caller as-is, never a mark.
func TestBareForbiddenAfterRemintIsRelayedNotCondemned(t *testing.T) {
	g := useGate(t)
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"message":"model not permitted"}`)
	})
	t.Setenv("UP1_KEY", "k1")
	srv := setupRouter(t,
		`{"default":"up1","providers":{"anthropic":{"up1":`+anthropicProvider(up.URL, "UP1_KEY", `{"opus":"m1"}`)+`}}}`,
		nil)

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1"}`, nil)
	b := body(t, resp)
	if resp.StatusCode != 403 || !strings.Contains(b, "model not permitted") {
		t.Fatalf("status %d body %s, want the upstream 403 relayed", resp.StatusCode, b)
	}
	if _, ok := g.marked("up1"); ok {
		t.Fatal("a bare 403 after the re-mint must not mark the provider")
	}
}

func TestFailoverOn429BeforeBytes(t *testing.T) {
	up1 := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	})
	up2 := newUpstream(t, ok)
	t.Setenv("UP1_KEY", "k1")
	t.Setenv("UP2_KEY", "k2")
	srv := setup(t, failoverOverlay(up1.URL, up2.URL))

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-haiku"}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if m := (*up2.body.Load())["model"]; m != "m2-haiku" {
		t.Errorf("failover model = %v, want same slot on up2", m)
	}
	// up1 is cooling down: the next request starts at up2.
	if resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-opus"}`, nil); resp.StatusCode != 200 {
		t.Fatalf("second status %d", resp.StatusCode)
	}
	if up1.hits.Load() != 1 || up2.hits.Load() != 2 {
		t.Fatalf("hits up1=%d up2=%d, want 1 and 2", up1.hits.Load(), up2.hits.Load())
	}
}

func TestLastAttemptRelaysUpstreamError(t *testing.T) {
	fail := func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(529)
		io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
	}
	up1, up2 := newUpstream(t, fail), newUpstream(t, fail)
	t.Setenv("UP1_KEY", "k1")
	t.Setenv("UP2_KEY", "k2")
	srv := setup(t, failoverOverlay(up1.URL, up2.URL))
	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-opus"}`, nil)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 529 || !strings.Contains(string(b), "busy") || up1.hits.Load() != 1 || up2.hits.Load() != 1 {
		t.Fatalf("got %d %s hits %d/%d", resp.StatusCode, b, up1.hits.Load(), up2.hits.Load())
	}
}

// Window and account exhaustion cool the provider without marking a model dead.
func TestExhaustionCoolsButDoesNotMarkDead(t *testing.T) {
	for name, msg := range map[string]string{
		"window":  "usage limit reached, try again in 60 seconds",
		"account": "402 payment required: insufficient_quota",
	} {
		t.Run(name, func(t *testing.T) {
			p := &providers.Provider{Name: "up1", Models: map[string]string{"opus": "m1-opus", "sonnet": "m1-sonnet"}}
			target := providers.Target{Provider: p, Model: "m1-opus"}
			vaulttest.Use(t) // the 60 s hold persists quota.json: keep it out of the shared home and later tests' dirs
			r := newRouter("s")
			t.Cleanup(r.bg.Wait)
			resp := &http.Response{
				StatusCode: 429,
				Header:     http.Header{"Retry-After": []string{"60"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"` + msg + `"}}`)),
			}
			moveOn, hard := r.failover(resp, target, false)
			if hard {
				t.Error("an exhaustion 429 must not be classified hard (ModelDenied), only IsQuota")
			}
			if !moveOn {
				t.Error("a non-last retryable 429 must still move on to the next ladder entry")
			}
			if r.isDead(target) {
				t.Error("failover must not mark the model dead on exhaustion")
			}
			if _, cooling := r.cool[p.Name]; !cooling {
				t.Error("failover must cool the provider on exhaustion")
			}
		})
	}
}

// A 400 "prompt is too long" is not retried and is rewritten to invalid_request_error quoting only the limit text.
func TestFailoverPromptTooLong(t *testing.T) {
	p := &providers.Provider{Name: "up1", Models: map[string]string{"opus": "m1"}}
	body := `{"message":"prompt is too long: 1177204 tokens > 1000000 maximum","secret":"leak-me"}`
	resp := &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
	moveOn, _ := newRouter("s").failover(resp, providers.Target{Provider: p, Model: "m1"}, false)
	if moveOn {
		t.Fatal("prompt-too-long must not fail over")
	}
	got, _ := io.ReadAll(resp.Body)
	var e struct {
		Error struct{ Type, Message string }
	}
	_ = json.Unmarshal(got, &e)
	if e.Error.Type != "invalid_request_error" ||
		e.Error.Message != "prompt is too long: 1177204 tokens > 1000000 maximum" ||
		strings.Contains(string(got), "leak-me") {
		t.Errorf("body = %s", got)
	}
}

// TestFailoverPromptTooLongNotOtherStatus pins that only a 400 is classified, and 400s without the text pass through.
func TestFailoverPromptTooLongNotOtherStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"500 with text":    {500, `{"message":"prompt is too long"}`},
		"400 other reason": {400, `{"message":"bad field"}`},
	} {
		t.Run(name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode: tc.status,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(tc.body)),
			}
			if tooLong(resp) {
				t.Fatal("must not classify")
			}
			got, _ := io.ReadAll(resp.Body)
			if string(got) != tc.body {
				t.Errorf("body altered: %s", got)
			}
		})
	}
}

// A dev session token is refused on /p/qa with 401 before any upstream call (authorized fails, so resolveTarget's
// 403 is never reached); on dev it reaches /serving-endpoints on the Messages, chat and Responses wires.
func TestSessionTokenConfinedToItsProfile(t *testing.T) {
	useGate(t)
	up := newRawUp(t, okRaw)
	t.Setenv("DEV_TOKEN", "dev-workspace-token")
	t.Setenv("QA_TOKEN", "qa-workspace-token")
	row := func(env string) string {
		serving := up.URL + "/serving-endpoints"
		return `{"preset":"databricks","host":"https://example.cloud.databricks.com","base_url":"` + serving +
			`/anthropic","routes":{"openai":"` + serving + `","responses":"` + serving + `"},` +
			`"auth":{"type":"key","env":"` + env + `"},"models":{"opus":"databricks-claude-opus-5-5"}}`
	}
	var routed *Router
	srv := setupRouter(t, `{"default":"dev","providers":{"databricks":{"dev":`+row("DEV_TOKEN")+`,"qa":`+
		row("QA_TOKEN")+`}}}`, func(r *Router) { routed = r })
	token, err := routed.sessions.mint([]string{"dev"}, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	const msg = `{"model":"databricks-claude-opus-5-5","max_tokens":5}`
	for _, path := range []string{"/p/qa/v1/messages", "/p/qa/openai/chat/completions", "/p/qa/responses/responses"} {
		hdr := map[string]string{"X-Api-Key": "", "Authorization": "Bearer " + token}
		resp := ptDo(t, http.MethodPost, srv.URL+path, msg, hdr)
		if got := body(t, resp); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("dev token on %s = %d %s, want 401", path, resp.StatusCode, got)
		}
	}
	if n := len(up.got()); n != 0 {
		t.Fatalf("dev token on qa reached the upstream %d times, want 0", n)
	}
	cases := []struct{ path, hdr, val, want string }{
		{"/p/dev/v1/messages", "X-Api-Key", token, "/serving-endpoints/anthropic/v1/messages"},
		{"/p/dev/openai/chat/completions", "Authorization", "Bearer " + token, "/serving-endpoints/chat/completions"},
		{"/p/dev/responses/responses", "Authorization", "Bearer " + token, "/serving-endpoints/responses"},
	}
	for i, c := range cases {
		hdr := map[string]string{"X-Api-Key": "", c.hdr: c.val}
		if resp := ptDo(t, http.MethodPost, srv.URL+c.path, msg, hdr); resp.StatusCode != http.StatusOK {
			t.Fatalf("dev token on %s = %d %s, want 200", c.path, resp.StatusCode, body(t, resp))
		}
		got := up.got()
		if len(got) != i+1 || got[i].uri != c.want {
			t.Fatalf("%s reached upstream %d times, last %+v; want %s", c.path, len(got), got[len(got)-1].uri, c.want)
		}
		if a := got[i].hdr.Get("Authorization"); a != "Bearer dev-workspace-token" {
			t.Errorf("%s: upstream Authorization is not dev's workspace credential", c.path)
		}
	}
}

// A token for {ultra, dev} on /p/ultra reaches a dev alias; a qa alias is 403 and /p/qa is 401, before any upstream.
func TestSessionTokenCoversItsProfileSet(t *testing.T) {
	useGate(t)
	up := newRawUp(t, okRaw)
	t.Setenv("ULTRA_KEY", "ultra-key")
	t.Setenv("DEV_TOKEN", "dev-workspace-token")
	t.Setenv("QA_TOKEN", "qa-workspace-token")
	row := func(env string) string {
		return `{"preset":"databricks","host":"https://example.cloud.databricks.com","base_url":"` + up.URL +
			`/serving-endpoints/anthropic","auth":{"type":"key","env":"` + env + `"},` +
			`"models":{"opus":"databricks-claude-opus-5-5"}}`
	}
	var routed *Router
	srv := setupRouter(t, `{"default":"ultra","providers":{"anthropic":{"ultra":`+
		anthropicProvider(up.URL, "ULTRA_KEY", `{"opus":"u-opus"}`)+`},"databricks":{"dev":`+row("DEV_TOKEN")+
		`,"qa":`+row("QA_TOKEN")+`}}}`, func(r *Router) { routed = r })
	token, err := routed.sessions.mint([]string{"ultra", "dev", "ultra"}, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	hdr := map[string]string{"X-Api-Key": token}
	cases := []struct {
		path, model string
		want        int
	}{
		{"/p/ultra/v1/messages", "dev:databricks-claude-opus-5-5", http.StatusOK},
		{"/p/ultra/v1/messages", "qa:databricks-claude-opus-5-5", http.StatusForbidden},
		{"/p/qa/v1/messages", "databricks-claude-opus-5-5", http.StatusUnauthorized},
	}
	for _, c := range cases {
		resp := post(t, srv.URL+c.path, `{"model":"`+c.model+`","max_tokens":5}`, hdr)
		if got := body(t, resp); resp.StatusCode != c.want {
			t.Errorf("%s model %s = %d %s, want %d", c.path, c.model, resp.StatusCode, got, c.want)
		}
	}
	got := up.got()
	if len(got) != 1 || got[0].hdr.Get("Authorization") != "Bearer dev-workspace-token" {
		t.Fatalf("upstream saw %d requests, want 1 on dev's workspace credential", len(got))
	}
}

func TestUnroutableLogin(t *testing.T) {
	p := &providers.Provider{
		Name:     "personal",
		Provider: "github",
		Protocol: providers.ProtocolOpenAI,
		BaseURL:  "https://x",
		Auth:     providers.Auth{Type: providers.AuthCLI, Login: "copilot"},
	}
	if why := unroutable(p, true); !strings.Contains(why, "copilot sign-in") {
		t.Errorf("a vendor login must never receive a forwarded client credential: %q", why)
	}
}

func TestPassthroughRefused(t *testing.T) {
	up := newUpstream(t, ok)
	srv := setup(t,
		`{"default":"sub","providers":{"anthropic":{"sub":{"protocol":"anthropic","base_url":"`+up.URL+`","auth":{"type":"cli"},"models":{"opus":"m"}}}}}`)
	resp := post(t, srv.URL+"/p/sub/v1/messages", `{"model":"m"}`, nil)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 400 || !strings.Contains(string(b), "passthrough") || up.hits.Load() != 0 {
		t.Fatalf("got %d %s hits=%d", resp.StatusCode, b, up.hits.Load())
	}
}

func TestPassthroughWithoutAnyCredentialIsRefused(t *testing.T) {
	tsrv, up := tlsUpstream(t)
	pointAnthropicHostAt(t, tsrv.URL)
	srv := setupRouter(t, subOverlay(tsrv.URL), func(r *Router) { r.client = tsrv.Client() })
	resp := post(t, srv.URL+"/p/sub/v1/messages", `{"model":"m"}`, nil)
	if resp.StatusCode != 400 || len(up.got()) != 0 {
		t.Fatalf("status %d hits %d, want 400 and no upstream call without a client login or vault token",
			resp.StatusCode, len(up.got()))
	}
}

// A rejected static key names the vault remedy and the mark, never a Databricks login.
func TestRejectedKeyHintsVaultSetNotDatabricksLogin(t *testing.T) {
	g := useGate(t)
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(http.StatusUnauthorized) })
	t.Setenv("UP1_KEY", "k1")
	srv := setupRouter(t,
		`{"default":"up1","providers":{"anthropic":{"up1":`+anthropicProvider(up.URL, "UP1_KEY", `{"opus":"m1"}`)+`}}}`,
		nil)

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1"}`, nil)
	b := body(t, resp)
	if resp.StatusCode != 401 || !strings.Contains(b, "run `claude-code vault set UP1_KEY`") ||
		strings.Contains(b, "databricks") {
		t.Fatalf("status %d body %s, want 401 with the vault set hint", resp.StatusCode, b)
	}
	if reason, ok := g.marked("up1"); !ok || !strings.Contains(reason, "vault set UP1_KEY") {
		t.Fatalf("mark = %q, %v; want the vault set hint", reason, ok)
	}
}

func TestLadderExhaustedReturns401WithLoginHint(t *testing.T) {
	g := useGate(t)
	up1 := newUpstream(t, ok)
	srv := setupRouter(t, `{"default":"up1","providers":{"databricks":{"up1":`+u2mRow(up1.URL)+`}}}`, nil)

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-opus"}`, nil)
	b := body(t, resp)
	if resp.StatusCode != 401 || !strings.Contains(b, `"authentication_error"`) ||
		!strings.Contains(b, "run `claude-code databricks login --profile up1`") {
		t.Fatalf("status %d body %s, want 401 authentication_error with the login hint", resp.StatusCode, b)
	}
	if up1.hits.Load() != 0 {
		t.Fatalf("upstream reached %d times without a credential", up1.hits.Load())
	}
	if _, ok := g.marked("up1"); !ok {
		t.Fatal("exhausting the ladder must still record the mark")
	}
}

func TestScopedAuthDeadProviderReturnsForbidden(t *testing.T) {
	gate := useGate(t)
	up1 := newUpstream(t, ok)
	up2 := newUpstream(t, ok)
	t.Setenv("UP1_KEY", "k1")
	t.Setenv("UP2_KEY", "k2")
	var routed *Router
	srv := setupRouter(t, failoverOverlay(up1.URL, up2.URL), func(r *Router) { routed = r })
	token, err := routed.sessions.mint([]string{"up1"}, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	gate.premark("up1") // attempts drops up1; session confinement then removes up2.

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-opus"}`, map[string]string{"X-Api-Key": token})
	got := body(t, resp)
	if resp.StatusCode != http.StatusForbidden ||
		!strings.Contains(got, "no provider in this session's scope is usable") {
		t.Fatalf("auth-dead scoped request = %d %s, want 403 without a panic", resp.StatusCode, got)
	}
	if up1.hits.Load() != 0 || up2.hits.Load() != 0 {
		t.Fatalf("scoped request escaped to upstreams: up1=%d up2=%d", up1.hits.Load(), up2.hits.Load())
	}
}
