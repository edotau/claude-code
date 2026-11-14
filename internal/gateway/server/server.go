package server

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/gateway/translate"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

const (
	maxBody         = 64 << 20
	defaultCooldown = 30 * time.Second
	maxCooldown     = 5 * time.Minute
	coolPersistMin  = 5 * time.Second  // shorter holds stay in this process: not worth a quota.json write
	credTTL         = 60 * time.Second // providers.CredentialPresent cache: one keyring probe per provider per TTL
	// router.LoadState cache: quota.json is read at most once per TTL, not per request
	authStateTTL = 5 * time.Second
	oauthMissTTL = 5 * time.Second    // a missing sealed token is re-read at most this often, so a new seal shows soon
	oauthBeta    = "oauth-2025-04-20" // the anthropic-beta Claude Code sends with a subscription bearer token
)

// Router authenticates loopback clients and forwards each Messages request along its attempt ladder.
type Router struct {
	secret string
	client *http.Client
	log    io.Writer
	now    func() time.Time

	sessions *sessionStore // lifetime-limited per-profile tokens, persisted across restarts
	clients  *clientTokens // named long-lived editor tokens, hashes sealed in the vault
	inflight atomic.Int64
	bg       sync.WaitGroup // background state writes (cooling, quota-dead); tests wait on it before cleanup

	regLoad  sync.Mutex // serializes providers.Load outside mu
	mu       sync.Mutex
	reg      *providers.Registry
	regStamp time.Time
	regRetry time.Time            // after a failed load, no new load before this instant
	cool     map[string]time.Time // provider → cooling-down until (after a 429)
	dead     map[string]time.Time // provider:model → quota-dead until (a hard 429: the key cannot use that model)
	deadAt   map[string]time.Time // provider:model → when its dead mark landed in quota.json
	cred     map[string]time.Time // provider name → present-credential cache expiry (credTTL)
	// provider name → generation, bumped on invalidate so a stale in-flight probe never resurrects it
	credGen map[string]int

	// cached router.LoadState() for attempts: auth marks and persisted quota-dead models
	authState router.State
	authAt    time.Time // when authState was loaded
	// bumped by condemn: a load begun before its mark is not cached
	authGen int
	// provider names this process marked auth-dead: cleared on their next success
	marked map[string]bool
	used   map[string]time.Time // provider name → last request through the router, for the keepalive loop
	ka     keepalive
	// scope|sha256 → uploaded file id (fileCacheTTL), guarded by mu
	files map[string]fileEntry
	// sha256 of the bearer the Files API refused; a new credential clears it
	filesOff  string
	filesTill time.Time // filesOff holds until then (filesOffTTL)
	// cached vault CLAUDE_CODE_OAUTH_TOKEN ("" = none sealed), for the passthrough fallback
	oauthTok string
	oauthAt  time.Time         // oauthTok is valid until oauthAt+credTTL, a miss until oauthAt+oauthMissTTL
	oauth    func() string     // reads the sealed subscription token; a seam for tests
	headers  map[string]string // validated HARNESS_HEADERS, canonical names
	// validated HARNESS_REQUEST_TAGS JSON, "" when unset or invalid
	tags string
}

// quotaDeadFor is how long a hard-quota model is skipped before the router tries it again.
const quotaDeadFor = 30 * time.Minute

func newRouter(secret string) *Router {
	r := &Router{secret: secret, client: guardedClient(), log: os.Stderr, now: time.Now,
		cool: map[string]time.Time{}, dead: map[string]time.Time{}, deadAt: map[string]time.Time{},
		cred: map[string]time.Time{}, credGen: map[string]int{},
		marked: map[string]bool{}, used: map[string]time.Time{}, ka: newKeepalive(), oauth: providers.HeadlessOAuthToken}
	r.sessions = newSessionStore(func() time.Time { return r.now() })
	r.clients = &clientTokens{load: loadClientHashes}
	r.tags = parseRequestTags(os.Getenv(requestTagsEnv), r.log)
	r.headers = parseHeaders(os.Getenv(headersEnv), r.log)
	return r
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	req, ok := stripVia(w, req)
	if !ok || r.serveGate(w, req) {
		return
	}
	r.inflight.Add(1)
	defer r.inflight.Add(-1)
	req = r.admit(req)
	if prov, dialect, rest, ok := parsePassthrough(req.URL.Path); ok {
		r.passthrough(w, req, prov, dialect, rest)
		return
	}
	session, count, ok := parsePath(req.URL.Path)
	if !ok || req.Method != http.MethodPost {
		r.refuse(w, req, http.StatusNotFound,
			"proxy: unsupported route "+req.Method+" "+req.URL.Path+" (want POST /p/<provider>/v1/messages)")
		return
	}
	start := r.now()
	sw := &statusWriter{ResponseWriter: w, rc: http.NewResponseController(w)}
	provider, model, attempts := r.route(sw, req, session, count)
	kind := ""
	if count {
		kind = " kind=count_tokens"
	}
	fmt.Fprintf(r.log, "%s %s %s %d %d %s%s%s%s%s\n", start.Format(time.RFC3339), provider, model, sw.status, attempts,
		time.Since(start).Round(time.Millisecond), kind, viaDiag(req), dispatchDiag(req), addBroken(errDiag(sw.errBuf), brokenDiag(sw)))
	sw.abortIfBroken()
}

// withLaunchIdentity puts the launch's IdentityHeader (--as, or headless) on req's context for the credential mint.
func withLaunchIdentity(req *http.Request) *http.Request {
	if id := req.Header.Get(gateway.IdentityHeader); id != "" {
		return req.WithContext(providers.WithIdentity(req.Context(), id))
	}
	return req
}

// refuse writes and logs an error for a request turned away before routing, which otherwise leaves no log line.
func (r *Router) refuse(w http.ResponseWriter, req *http.Request, status int, msg string) {
	fmt.Fprintf(r.log, "%s refused %d %s %s: %s\n", r.now().Format(time.RFC3339), status, req.Method, req.URL.Path, msg)
	writeError(w, status, msg)
}

// serveGate answers the host check, health, session and auth-failure requests; true when it wrote the response.
func (r *Router) serveGate(w http.ResponseWriter, req *http.Request) bool {
	if !loopbackHost(req.Host) {
		r.refuse(w, req, http.StatusForbidden, "proxy: Host must be 127.0.0.1, localhost or [::1]")
		return true
	}
	if req.URL.Path == "/healthz" {
		r.health(w)
		return true
	}
	if helloProbe(req) { // Claude Code's keyless startup reachability check: answered here, never forwarded
		w.WriteHeader(http.StatusOK)
		return true
	}
	if req.URL.Path == gateway.SessionPath || req.URL.Path == gateway.SessionRevokePath {
		r.sessionRoute(w, req)
		return true
	}
	if !r.authorized(req) {
		r.refuseUnauthorized(w, req)
		return true
	}
	return false
}

// helloProbe reports a HEAD or GET of /p/<profile>/api/hello.
func helloProbe(req *http.Request) bool {
	_, rest, ok := parseRoute(req.URL.Path)
	return ok && rest == "api/hello" && (req.Method == http.MethodHead || req.Method == http.MethodGet)
}

// sessionRoute serves the mint/revoke paths to the real client secret only.
func (r *Router) sessionRoute(w http.ResponseWriter, req *http.Request) {
	if !r.secretOK(req) { // a session token never mints or revokes
		r.refuse(w, req, http.StatusUnauthorized, "proxy: missing or wrong client secret")
		return
	}
	r.serveSession(w, req)
}

// stripClientAuth drops the proxy secret headers; Authorization survives only as the client's own credential.
func stripClientAuth(req *http.Request) {
	if req.Header.Get(gateway.ClientHeader) == "" {
		req.Header.Del("Authorization")
		req.Header.Del("X-Api-Key")
	}
	req.Header.Del(gateway.ClientHeader)
}

// fallbackError keeps audit metadata separate from upstream error text, which may contain credentials.
type fallbackError struct {
	error
	status int
	hard   bool   // a 429 that denies the model outright (quota 0), not a transient rate limit
	auth   bool   // the credential was condemned (login required or rejected after a re-mint)
	text   string // the 401 text after "proxy: <p>: " when the row names its own login command; else authHint's
}

func (e *fallbackError) Unwrap() error { return e.error }

// route resolves the target and walks the attempt ladder; it returns what the log line names.
func (r *Router) route(w *statusWriter, in *http.Request, session string, count bool) (string, string, int) {
	body, ok := readBody(w, in)
	if !ok {
		return session, "-", 0
	}
	model := passthroughModel(body)
	if model == "" {
		writeError(w, http.StatusBadRequest, "proxy: request body must be JSON with a model")
		return session, "-", 0
	}
	reg, err := r.registry()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "proxy: "+err.Error())
		return session, model, 0
	}
	target, ok := resolveTarget(w, in, reg, session, model)
	if !ok {
		return session, model, 0
	}
	clientAuth := in.Header.Get("Authorization") != "" ||
		r.oauthToken() != "" // the sealed token authenticates passthrough
	if msg := unroutable(target.Provider, clientAuth); msg != "" {
		writeError(w, http.StatusBadRequest, "proxy: "+msg)
		return target.Provider.Name, target.Model, 0
	}
	clientModel := models.Strip1M(model)
	ladder := r.attempts(reg, target, clientAuth)
	ladder = confineLadder(in, ladder)
	return r.walkAttempts(w, in, body, ladder, clientModel, count)
}

func (r *Router) walkAttempts(w *statusWriter, in *http.Request, body []byte, ladder []providers.Target,
	clientModel string, count bool) (string, string, int) {
	var lastErr error
	for i := 0; i < len(ladder); i++ { // indexed: a hard-quota failure inserts the demoted model right after t
		t := ladder[i]
		last := i == len(ladder)-1
		done, err := r.try(w, in, body, t, clientModel, count, last)
		if done {
			r.clearOnSuccess(t.Provider, w.status)
			return t.Provider.Name, t.Model, i + 1
		}
		lastErr = err
		if in.Context().Err() != nil {
			return t.Provider.Name, t.Model, i + 1
		}
		ladder = r.noteFailure(ladder, i, err)
	}
	return exhausted(w, ladder, lastErr)
}

// noteFailure logs attempt i's failure; a hard-quota one returns the ladder with a demoted model inserted after i.
func (r *Router) noteFailure(ladder []providers.Target, i int, err error) []providers.Target {
	t := ladder[i]
	var failure *fallbackError
	isFailure := errors.As(err, &failure)
	if isFailure && failure.hard {
		if d, ok := r.demotion(t); ok {
			fmt.Fprintf(r.log, "proxy: %s %s is quota-dead for this key, demoting to %s\n", t.Provider.Name, t.Model,
				d.Model)
			audit.Log(audit.RouterFallback, t.Provider.Name, "status=429 quota; demoted to "+d.Model)
			return slices.Insert(ladder, i+1, d)
		}
	}
	if i < len(ladder)-1 {
		fmt.Fprintf(r.log, "proxy: %s %s failed, failing over: %v\n", t.Provider.Name, t.Model, err)
		if isFailure {
			detail := "connect error"
			if failure.status != 0 {
				detail = fmt.Sprintf("status=%d", failure.status)
			}
			audit.Log(audit.RouterFallback, t.Provider.Name, detail)
		}
	}
	return ladder
}

// attempts is the target then each usable fallback provider; cooling-down providers sink to the end.
func (r *Router) attempts(reg *providers.Registry, target providers.Target, clientAuth bool) []providers.Target {
	out := r.failoverLadder(reg, target, clientAuth)
	// A model this key cannot use at all is replaced by its provider's next slot up front.
	for i, t := range out {
		if r.isDead(t) {
			if d, ok := r.demotion(t); ok {
				out[i] = d
			}
		}
	}
	out = r.withoutAuthMarked(out)
	state, now := r.routeState(), r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	cooling := func(t providers.Target) bool { // this process's hold, else one another process persisted
		return now.Before(r.cool[t.Provider.Name]) || now.Before(state.CoolingUntil(router.AccountOf(t.Provider)))
	}
	sort.SliceStable(out, func(i, j int) bool { return !cooling(out[i]) && cooling(out[j]) })
	return out
}

// failoverLadder is the target followed by one failover target per usable fallback provider, in registry order.
func (r *Router) failoverLadder(reg *providers.Registry, target providers.Target, clientAuth bool) []providers.Target {
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
	return out
}

// withoutAuthMarked drops targets whose provider has a live auth mark; when that empties the ladder it is kept whole.
func (r *Router) withoutAuthMarked(out []providers.Target) []providers.Target {
	state, now := r.routeState(), r.now()
	kept := make([]providers.Target, 0, len(out))
	for _, t := range out {
		if _, ok := state.AuthMarked(router.AccountOf(t.Provider), now); !ok {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		return out
	}
	return kept
}

// routeState is the route state, re-read from quota.json at most once per authStateTTL; callers only read it.
func (r *Router) routeState() router.State {
	state, _ := r.routeStateAt()
	return state
}

// routeStateAt is routeState plus when that state's read began.
func (r *Router) routeStateAt() (router.State, time.Time) {
	r.mu.Lock()
	now, state, at, gen := r.now(), r.authState, r.authAt, r.authGen
	fresh := state != nil && now.Sub(at) < authStateTTL
	r.mu.Unlock()
	if fresh {
		return state, at
	}
	state = router.LoadState() // read quota.json without holding mu
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.authGen == gen {
		r.authState, r.authAt = state, now
	}
	return state, now
}

// condemn records a provider as auth-dead in the route state; best-effort, the caller moves on regardless.
func (r *Router) condemn(p *providers.Provider, v providers.AuthVerdict) {
	ran, err := router.TryMarkAuth(router.AccountOf(p), "auth "+string(v)+": "+authHint(p))
	if err != nil || !ran {
		fmt.Fprintf(r.log, "proxy: mark %s auth-dead: %v\n", p.Name, cmp.Or(err, errors.New("state lock busy")))
		return
	}
	r.mu.Lock()
	r.marked[p.Name] = true
	r.authAt = time.Time{} // the next attempts() re-reads the state and sees the mark
	r.authGen++
	r.mu.Unlock()
}

// clearOnSuccess lifts a mark this process set once the provider answers with a 2xx.
func (r *Router) clearOnSuccess(p *providers.Provider, status int) {
	if status < 200 || status > 299 {
		return
	}
	r.mu.Lock()
	was := r.marked[p.Name]
	delete(r.marked, p.Name)
	r.mu.Unlock()
	if was {
		_ = router.ClearAuth(router.AccountOf(p)) // best-effort: the TTL backstop expires a missed clear
	}
}

// oauthToken is the vault's sealed subscription token, cached for credTTL, or oauthMissTTL when none is sealed.
func (r *Router) oauthToken() string {
	r.mu.Lock()
	ttl := credTTL
	if r.oauthTok == "" {
		ttl = oauthMissTTL
	}
	if !r.oauthAt.IsZero() && r.now().Sub(r.oauthAt) < ttl {
		tok := r.oauthTok
		r.mu.Unlock()
		return tok
	}
	r.mu.Unlock()
	tok := r.oauth()
	r.mu.Lock()
	r.oauthTok, r.oauthAt = tok, r.now()
	r.mu.Unlock()
	return tok
}

// try runs one attempt. done=false means nothing was written and err says why (the caller may fail over).
func (r *Router) try(w *statusWriter, in *http.Request, body []byte, t providers.Target, clientModel string,
	count, last bool) (bool, error) {
	// Databricks serving endpoints have no count_tokens path (400 BAD_REQUEST), so the estimate is answered here.
	route, ok := providers.AnthropicRoute(t.Provider, t.Model)
	if ok && (!count || t.Provider.Preset != providers.PresetDatabricks) {
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
	wire, servable, why := providers.WireFor(t.Provider, t.Model)
	if !servable {
		writeError(w, http.StatusBadRequest, why)
		return true, nil
	}
	if wire == providers.ProtocolResponses { // the translate path has no Responses wire
		writeError(w, http.StatusBadRequest, providers.ClaudeCodeError(t.Provider, t.Model).Error())
		return true, nil
	}
	return r.tryOpenAI(w, in, body, t, clientModel, last)
}

func (r *Router) tryAnthropic(w http.ResponseWriter, in *http.Request, body []byte, t providers.Target, route string,
	count, last bool) (bool, error) {
	p := t.Provider
	url := route + "/v1/messages"
	if count {
		url += "/count_tokens"
	}
	if in.URL.RawQuery != "" {
		url += "?" + in.URL.RawQuery
	}
	out, err := withModel(r.rewriteFiles(in, p, route, body, count), t.Model)
	if err != nil {
		return false, err
	}
	resp, err := r.send(in, p, url, out, func(h http.Header) {
		copyRequestHeaders(h, in.Header)
		if p.Auth.Method() == providers.AuthCLI {
			r.passthroughAuth(h, in, route)
		}
	})
	if err != nil {
		return false, err
	}
	if moveOn, hard := r.failover(resp, t, last); moveOn {
		return false, &fallbackError{error: errors.New(resp.Status), status: resp.StatusCode, hard: hard}
	}
	if !count && metered(p) {
		var capture headTailBuffer
		orig := resp.Body
		resp.Body = teeReadCloser{io.TeeReader(orig, &capture), orig}
		relay(w, resp)
		logLedgerUsage(in, p, t.Model, scrapeAnthropicUsage(capture.Bytes()))
		return true, nil
	}
	relay(w, resp)
	return true, nil
}

func (r *Router) tryOpenAI(w *statusWriter, in *http.Request, body []byte, t providers.Target, clientModel string,
	last bool) (bool, error) {
	p := t.Provider
	route, _ := p.Route(providers.ProtocolOpenAI)
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
		writeJSON(w, resp.StatusCode, translate.FromChatError(resp.StatusCode, b))
		return true, nil
	}
	return r.relayChat(w, in, resp, t, clientModel, stream)
}

// send POSTs body with the provider's auth; a 401/403 refreshes the credential and replays once.
func (r *Router) send(in *http.Request, p *providers.Provider, url string, body []byte,
	hdr func(http.Header)) (*http.Response, error) {
	return r.sendMethod(in, http.MethodPost, p, url, body, hdr)
}

// sendMethod is send with an explicit HTTP method (GET passthrough carries no body).
func (r *Router) sendMethod(in *http.Request, method string, p *providers.Provider, url string, body []byte,
	hdr func(http.Header)) (*http.Response, error) {
	ctx := in.Context()
	r.touch(p.Name)
	do := func(credential string) (*http.Response, error) {
		return r.sendRequest(in, method, p, url, body, hdr, credential)
	}
	resp, err := do("")
	if err != nil {
		return nil, r.authFailure(ctx, p, err)
	}
	resp.Body = newIdleBody(resp.Body, idleTimeout) // before invalidToken's peek, so a stalled 400 body cannot hang
	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || invalidToken(resp, p)) &&
		p.Auth.HeaderStyle() != "" {
		drain(resp)
		r.invalidateCredential(
			p.Name,
		) // invalidate before refreshing: a Refresh failure must not leave a stale "present" cached
		credential, err := providers.Refresh(ctx, p)
		if err != nil {
			return nil, r.authFailure(ctx, p, err)
		}
		if resp, err = do(credential); err != nil {
			return nil, err
		}
		resp.Body = newIdleBody(resp.Body, idleTimeout)
		if rejectedAfterRemint(resp, p) { // a policy/WAF 403 is relayed to the client, never condemned
			drain(resp)
			return nil, r.condemned(ctx, p, providers.VerdictRejected, "credential rejected after re-mint")
		}
	}
	return resp, nil
}
