// Package router is the loopback model router Claude Code talks to when a session needs translation,
// cross-provider slots, or failover.
package router

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/translate"
)

const (
	maxBody         = 64 << 20
	defaultCooldown = 30 * time.Second
	maxCooldown     = 5 * time.Minute
	credTTL         = 60 * time.Second // providers.CredentialPresent cache: one keyring probe per provider per TTL
)

// idleTimeout cuts a wedged upstream stream; a var so tests need not wait 10 minutes.
var idleTimeout = 10 * time.Minute

// retryable statuses move to the next attempt when nothing has been written to the client yet.
var retryable = map[int]bool{429: true, 500: true, 502: true, 503: true, 504: true, 529: true}

// Router authenticates loopback clients and forwards each Messages request along its attempt ladder.
type Router struct {
	secret string
	client *http.Client
	log    io.Writer
	now    func() time.Time

	mu       sync.Mutex
	reg      *providers.Registry
	regStamp time.Time
	cool     map[string]time.Time  // provider → cooling-down until (after a 429)
	dead     map[string]time.Time  // provider:model → quota-dead until (a hard 429: the key cannot use that model)
	cred     map[string]credResult // provider name → cached providers.CredentialPresent, TTL credTTL
	credGen  map[string]int        // provider name → generation, bumped on invalidate so a stale in-flight probe never resurrects it
}

// credResult is one provider's cached CredentialPresent outcome.
type credResult struct {
	present bool
	until   time.Time
}

// quotaDeadFor is how long a hard-quota model is skipped before the router tries it again.
const quotaDeadFor = 30 * time.Minute

func newRouter(secret string) *Router {
	return &Router{secret: secret, client: guardedClient(), log: os.Stderr, now: time.Now,
		cool: map[string]time.Time{}, dead: map[string]time.Time{}, cred: map[string]credResult{}, credGen: map[string]int{}}
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if !loopbackHost(req.Host) {
		writeError(w, http.StatusForbidden, "router: Host must be 127.0.0.1, localhost or [::1]")
		return
	}
	if req.URL.Path == "/healthz" {
		_, _ = io.WriteString(w, "ok\n")
		return
	}
	if !r.authorized(req) {
		writeError(w, http.StatusUnauthorized, "router: missing or wrong client secret (apiKeyHelper: claude-code token --router)")
		return
	}
	// Past auth, Authorization survives only as the client's own credential (secret sent in ClientHeader).
	if req.Header.Get(ClientHeader) == "" {
		req.Header.Del("Authorization")
		req.Header.Del("X-Api-Key")
	}
	req.Header.Del(ClientHeader)
	session, count, ok := parsePath(req.URL.Path)
	if !ok || req.Method != http.MethodPost {
		writeError(w, http.StatusNotFound, "router: unsupported route "+req.Method+" "+req.URL.Path+" (want POST /p/<provider>/v1/messages)")
		return
	}
	start := r.now()
	sw := &statusWriter{ResponseWriter: w, rc: http.NewResponseController(w)}
	provider, model, attempts := r.route(sw, req, session, count)
	fmt.Fprintf(r.log, "%s %s %s %d %d %s\n", start.Format(time.RFC3339), provider, model, sw.status, attempts, time.Since(start).Round(time.Millisecond))
}

// fallbackError keeps audit metadata separate from upstream error text, which may contain credentials.
type fallbackError struct {
	error
	status int
	hard   bool // a 429 that denies the model outright (quota 0), not a transient rate limit
}

func (e *fallbackError) Unwrap() error { return e.error }

// route resolves the target and walks the attempt ladder; it returns what the log line names.
func (r *Router) route(w *statusWriter, in *http.Request, session string, count bool) (string, string, int) {
	body, err := io.ReadAll(io.LimitReader(in.Body, maxBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "router: read body: "+err.Error())
		return session, "-", 0
	}
	if len(body) > maxBody {
		writeError(w, http.StatusRequestEntityTooLarge, "router: request body over 64MiB")
		return session, "-", 0
	}
	var head struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &head); err != nil || head.Model == "" {
		writeError(w, http.StatusBadRequest, "router: request body must be JSON with a model")
		return session, "-", 0
	}
	reg, err := r.registry()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "router: "+err.Error())
		return session, head.Model, 0
	}
	sess, err := reg.Get(session)
	if err != nil {
		writeError(w, http.StatusBadRequest, "router: "+err.Error())
		return session, head.Model, 0
	}
	target := providers.ParseTarget(reg, head.Model, sess)
	clientAuth := in.Header.Get("Authorization") != ""
	if msg := unroutable(target.Provider, clientAuth); msg != "" {
		writeError(w, http.StatusBadRequest, "router: "+msg)
		return target.Provider.Name, target.Model, 0
	}
	clientModel := models.Strip1M(head.Model)
	ladder := r.attempts(reg, target, clientAuth)
	var lastErr error
	for i := 0; i < len(ladder); i++ { // indexed: a hard-quota failure inserts the demoted model right after t
		t := ladder[i]
		last := i == len(ladder)-1
		done, err := r.try(w, in, body, t, clientModel, count, last)
		if done {
			return t.Provider.Name, t.Model, i + 1
		}
		lastErr = err
		if in.Context().Err() != nil {
			return t.Provider.Name, t.Model, i + 1
		}
		var failure *fallbackError
		isFailure := errors.As(err, &failure)
		if isFailure && failure.hard {
			if d, ok := r.demotion(t); ok {
				ladder = slices.Insert(ladder, i+1, d)
				fmt.Fprintf(r.log, "router: %s %s is quota-dead for this key, demoting to %s\n", t.Provider.Name, t.Model, d.Model)
				audit.Log(audit.RouterFallback, t.Provider.Name, "status=429 quota; demoted to "+d.Model)
				continue
			}
		}
		if i < len(ladder)-1 {
			fmt.Fprintf(r.log, "router: %s %s failed, failing over: %v\n", t.Provider.Name, t.Model, err)
			if isFailure {
				detail := "connect error"
				if failure.status != 0 {
					detail = fmt.Sprintf("status=%d", failure.status)
				}
				audit.Log(audit.RouterFallback, t.Provider.Name, detail)
			}
		}
	}
	t := ladder[len(ladder)-1]
	writeError(w, http.StatusBadGateway, fmt.Sprintf("router: %s: %v", t.Provider.Name, lastErr))
	return t.Provider.Name, t.Model, len(ladder)
}

// unroutable explains why the router cannot serve p at all, or returns "".
func unroutable(p *providers.Provider, clientAuth bool) string {
	if p.Auth.Login != "" {
		return fmt.Sprintf("provider %s is the %s sign-in; only claude-code run %s can use it", p.Name, p.Auth.Login, p.Auth.Login)
	}
	if p.Auth.Type == providers.AuthPassthrough && !clientAuth {
		return fmt.Sprintf("provider %s uses passthrough auth but the request carries no client credential; launch it as the session provider (claude-code use %s) so its login reaches the router", p.Name, p.Name)
	}
	_, a := p.Route(providers.DialectAnthropic)
	_, o := p.Route(providers.DialectOpenAI)
	if !a && !o {
		return fmt.Sprintf("provider %s has no anthropic or openai route (dialects: %s)", p.Name, strings.Join(p.Dialects(), ","))
	}
	return ""
}

// attempts is the target then each usable fallback provider; cooling-down providers sink to the end.
func (r *Router) attempts(reg *providers.Registry, target providers.Target, clientAuth bool) []providers.Target {
	out := []providers.Target{target}
	seen := map[*providers.Provider]bool{target.Provider: true}
	for _, name := range reg.Fallback {
		p := reg.Providers[name]
		if p == nil || seen[p] || unroutable(p, clientAuth) != "" || !r.credentialPresent(p) {
			continue
		}
		if ft, ok := providers.FailoverTarget(target.Provider, target.Model, p); ok {
			seen[p] = true
			out = append(out, ft)
		}
	}
	// A model this key cannot use at all is replaced by its provider's next slot up front.
	for i, t := range out {
		if r.isDead(t) {
			if d, ok := r.demotion(t); ok {
				out[i] = d
			}
		}
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	cooling := func(t providers.Target) bool { return now.Before(r.cool[t.Provider.Name]) }
	sort.SliceStable(out, func(i, j int) bool { return !cooling(out[i]) && cooling(out[j]) })
	return out
}

// demotion walks t's provider down the slot ladder to the first model not known to be quota-dead.
func (r *Router) demotion(t providers.Target) (providers.Target, bool) {
	for d, ok := providers.Demote(t.Provider, t.Model); ok; d, ok = providers.Demote(t.Provider, d.Model) {
		if !r.isDead(d) {
			return d, true
		}
	}
	return providers.Target{}, false
}

// credentialPresent caches providers.CredentialPresent per provider name for credTTL, present results
// only: an env-auth provider with no process env var falls through to a keyring lookup (a secret-tool
// subprocess per call), and a present credential is probed once per TTL instead of every routed request.
// An absent credential is re-checked every call, so a freshly added fallback credential is picked up
// immediately rather than staying hidden for up to credTTL.
func (r *Router) credentialPresent(p *providers.Provider) bool {
	now := r.now()
	r.mu.Lock()
	if c, ok := r.cred[p.Name]; ok && now.Before(c.until) {
		r.mu.Unlock()
		return c.present
	}
	startGen := r.credGen[p.Name]
	r.mu.Unlock()
	if !providers.CredentialPresent(p) {
		return false
	}
	r.storeCredentialPresent(p.Name, startGen, now.Add(credTTL))
	return true
}

// storeCredentialPresent records a probe's "present" outcome only if name's generation has not advanced
// since the probe started (startGen): an invalidation racing with an in-flight probe must win, so a probe
// that started before it must never resurrect the cache afterward.
func (r *Router) storeCredentialPresent(name string, startGen int, until time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.credGen[name] != startGen {
		return
	}
	r.cred[name] = credResult{present: true, until: until}
}

// invalidateCredential drops a provider's cached presence so the next attempt re-probes it, and bumps its
// generation so any probe already in flight cannot store a stale "present" after this call returns.
func (r *Router) invalidateCredential(name string) {
	r.mu.Lock()
	delete(r.cred, name)
	r.credGen[name]++
	r.mu.Unlock()
}

func (r *Router) isDead(t providers.Target) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now().Before(r.dead[t.Provider.Name+":"+t.Model])
}

func (r *Router) markDead(t providers.Target) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dead[t.Provider.Name+":"+t.Model] = r.now().Add(quotaDeadFor)
}

// try runs one attempt. done=false means nothing was written and err says why (the caller may fail over).
func (r *Router) try(w http.ResponseWriter, in *http.Request, body []byte, t providers.Target, clientModel string, count, last bool) (bool, error) {
	if route, ok := t.Provider.Route(providers.DialectAnthropic); ok {
		return r.tryAnthropic(w, in, body, t, route, count, last)
	}
	if count {
		out, err := translate.CountTokens(body)
		if err != nil {
			return false, err
		}
		writeJSON(w, http.StatusOK, out)
		return true, nil
	}
	return r.tryOpenAI(w, in, body, t, clientModel, last)
}

func (r *Router) tryAnthropic(w http.ResponseWriter, in *http.Request, body []byte, t providers.Target, route string, count, last bool) (bool, error) {
	p := t.Provider
	url := route + "/v1/messages"
	if count {
		url += "/count_tokens"
	}
	if in.URL.RawQuery != "" {
		url += "?" + in.URL.RawQuery
	}
	out, err := withModel(body, t.Model)
	if err != nil {
		return false, err
	}
	resp, err := r.send(in, p, url, out, func(h http.Header) {
		copyRequestHeaders(h, in.Header)
		if p.Auth.Type == providers.AuthPassthrough {
			h.Set("Authorization", in.Header.Get("Authorization"))
		}
	})
	if err != nil {
		return false, err
	}
	if moveOn, hard := r.failover(resp, t, last); moveOn {
		return false, &fallbackError{error: errors.New(resp.Status), status: resp.StatusCode, hard: hard}
	}
	relay(w, resp)
	return true, nil
}

func (r *Router) tryOpenAI(w http.ResponseWriter, in *http.Request, body []byte, t providers.Target, clientModel string, last bool) (bool, error) {
	p := t.Provider
	route, _ := p.Route(providers.DialectOpenAI)
	out, stream, err := translate.ToChatRequest(body, t.Model)
	if err != nil {
		return false, err
	}
	resp, err := r.send(in, p, route+"/chat/completions", out, func(h http.Header) {
		h.Set("Content-Type", "application/json")
		if stream {
			h.Set("Accept", "text/event-stream")
		}
	})
	if err != nil {
		return false, err
	}
	if moveOn, hard := r.failover(resp, t, last); moveOn {
		return false, &fallbackError{error: errors.New(resp.Status), status: resp.StatusCode, hard: hard}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		eb := translate.FromChatError(resp.StatusCode, b)
		if eb == nil {
			eb = errorBody(resp.StatusCode, p.Name+": "+strings.TrimSpace(string(b)))
		}
		writeJSON(w, resp.StatusCode, eb)
		return true, nil
	}
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		if err := translate.StreamChat(w, flusher(w), resp.Body, clientModel); err != nil && in.Context().Err() == nil {
			fmt.Fprintf(r.log, "router: %s stream translation: %v\n", p.Name, err)
		}
		return true, nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return false, err
	}
	ab, err := translate.FromChatResponse(b, clientModel)
	if err != nil {
		return false, err
	}
	writeJSON(w, http.StatusOK, ab)
	return true, nil
}

// send POSTs body with the provider's auth; a 401/403 refreshes the credential and replays once.
func (r *Router) send(in *http.Request, p *providers.Provider, url string, body []byte, hdr func(http.Header)) (*http.Response, error) {
	ctx := in.Context()
	do := func(credential string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		hdr(req.Header)
		if credential == "" {
			if err := providers.Authorize(ctx, p, req.Header); err != nil {
				return nil, err
			}
		} else {
			for k, v := range p.Headers {
				req.Header.Set(k, v)
			}
			req.Header.Del("Authorization")
			req.Header.Del("X-Api-Key")
			if p.Auth.Type == providers.AuthAPIKey {
				req.Header.Set("X-Api-Key", credential)
			} else {
				req.Header.Set("Authorization", "Bearer "+credential)
			}
		}
		resp, err := r.client.Do(req)
		if err != nil {
			return resp, &fallbackError{error: err}
		}
		return resp, nil
	}
	resp, err := do("")
	if err != nil {
		return nil, err
	}
	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) &&
		(p.Auth.Type == providers.AuthAPIKey || p.Auth.Type == providers.AuthBearer) {
		drain(resp)
		r.invalidateCredential(p.Name) // invalidate before refreshing: a Refresh failure must not leave a stale "present" cached
		credential, err := providers.Refresh(ctx, p)
		if err != nil {
			return nil, err
		}
		if resp, err = do(credential); err != nil {
			return nil, err
		}
	}
	resp.Body = newIdleBody(resp.Body, idleTimeout)
	return resp, nil
}

// failover classifies a failed response: a hard-quota 429 marks the model dead (and moves on even on the last
// attempt when the provider has a lower slot); a transient 429 cools the provider; retryable statuses move on
// unless this was the last attempt. moveOn means the response was drained and the caller may try the next target.
func (r *Router) failover(resp *http.Response, t providers.Target, last bool) (moveOn, hard bool) {
	p := t.Provider
	if resp.StatusCode == http.StatusTooManyRequests {
		peek, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(peek), resp.Body), resp.Body}
		if hard = providers.HardQuota(string(peek)); hard {
			r.markDead(t)
			if _, ok := r.demotion(t); ok {
				drain(resp)
				return true, true
			}
		} else {
			d := retryAfter(resp.Header.Get("Retry-After"), r.now())
			r.mu.Lock()
			r.cool[p.Name] = r.now().Add(d)
			r.mu.Unlock()
		}
	}
	if last || !retryable[resp.StatusCode] {
		return false, hard
	}
	drain(resp)
	return true, hard
}

func retryAfter(v string, now time.Time) time.Duration {
	d := defaultCooldown
	if s, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && s > 0 {
		d = time.Duration(s) * time.Second
	} else if at, err := http.ParseTime(v); err == nil && at.After(now) {
		d = at.Sub(now)
	}
	return min(d, maxCooldown)
}

// registry reloads providers.json when its mtime changes, keeping the last good table on a bad edit.
func (r *Router) registry() (*providers.Registry, error) {
	var stamp time.Time
	if fi, err := os.Stat(paths.ProvidersOverlay()); err == nil {
		stamp = fi.ModTime()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reg != nil && stamp.Equal(r.regStamp) {
		return r.reg, nil
	}
	reg, err := providers.Load()
	if err != nil {
		if r.reg == nil {
			return nil, err
		}
		fmt.Fprintf(r.log, "router: keeping previous registry: %v\n", err)
		reg = r.reg
	}
	r.reg, r.regStamp = reg, stamp
	return reg, nil
}

func (r *Router) authorized(req *http.Request) bool {
	got := req.Header.Get(ClientHeader)
	if got == "" {
		got = req.Header.Get("X-Api-Key")
	}
	if got == "" {
		got, _ = strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	}
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(r.secret)) == 1
}

func loopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "::1" || strings.EqualFold(host, "localhost")
}

// parsePath splits /p/<provider>/v1/messages[/count_tokens].
func parsePath(path string) (provider string, count, ok bool) {
	rest, ok := strings.CutPrefix(path, "/p/")
	if !ok {
		return "", false, false
	}
	provider, rest, _ = strings.Cut(rest, "/")
	switch "/" + rest {
	case "/v1/messages":
		return provider, false, provider != ""
	case "/v1/messages/count_tokens":
		return provider, true, provider != ""
	}
	return "", false, false
}

// withModel rewrites the top-level model; an unchanged id forwards the client's bytes verbatim.
func withModel(body []byte, model string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	var cur string
	if json.Unmarshal(m["model"], &cur) == nil && cur == model {
		return body, nil
	}
	m["model"], _ = json.Marshal(model)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// copyRequestHeaders forwards only wire-relevant headers; client auth is added back for passthrough providers only.
func copyRequestHeaders(dst, src http.Header) {
	for k, vs := range src {
		ck := http.CanonicalHeaderKey(k)
		if strings.HasPrefix(ck, "Anthropic-") || ck == "Content-Type" || ck == "Accept" {
			dst[ck] = append([]string(nil), vs...)
		}
	}
}

var hopHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true,
	"Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true, "Content-Length": true,
}

func relay(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		if !hopHeaders[http.CanonicalHeaderKey(k)] {
			w.Header()[k] = vs
		}
	}
	w.WriteHeader(resp.StatusCode)
	flush := flusher(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			flush()
		}
		if err != nil {
			return
		}
	}
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}

func flusher(w http.ResponseWriter) func() {
	if f, ok := w.(http.Flusher); ok {
		return f.Flush
	}
	return func() {}
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody(status, msg))
}

// errorBody is the Anthropic error envelope Claude Code parses.
func errorBody(status int, msg string) []byte {
	typ := "api_error"
	switch status {
	case 400, 413:
		typ = "invalid_request_error"
	case 401:
		typ = "authentication_error"
	case 403:
		typ = "permission_error"
	case 404:
		typ = "not_found_error"
	case 429:
		typ = "rate_limit_error"
	case 529:
		typ = "overloaded_error"
	}
	b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": msg}})
	return b
}

type statusWriter struct {
	http.ResponseWriter
	status int
	rc     *http.ResponseController // cached: constructing one per Flush call allocated on every SSE chunk
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Flush() { _ = s.rc.Flush() }

// idleBody closes the upstream body when no Read completes within idle, unblocking a wedged stream.
type idleBody struct {
	rc    io.ReadCloser
	idle  time.Duration
	timer *time.Timer
}

func newIdleBody(rc io.ReadCloser, idle time.Duration) *idleBody {
	return &idleBody{rc: rc, idle: idle, timer: time.AfterFunc(idle, func() { _ = rc.Close() })}
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.timer.Reset(b.idle)
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	return b.rc.Close()
}
