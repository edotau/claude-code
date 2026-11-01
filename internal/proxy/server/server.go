package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/translate"
)

const (
	maxBody         = 64 << 20
	defaultCooldown = 30 * time.Second
	maxCooldown     = 5 * time.Minute
	credTTL         = 60 * time.Second   // providers.CredentialPresent cache: one keyring probe per provider per TTL
	authStateTTL    = 5 * time.Second    // router.LoadState cache: quota.json is read at most once per TTL, not per request
	oauthBeta       = "oauth-2025-04-20" // the anthropic-beta Claude Code sends with a subscription bearer token
)

// Router authenticates loopback clients and forwards each Messages request along its attempt ladder.
type Router struct {
	secret string
	client *http.Client
	log    io.Writer
	now    func() time.Time

	sessions *sessionStore // lifetime-limited per-profile tokens, persisted across restarts
	inflight atomic.Int64

	mu       sync.Mutex
	reg      *providers.Registry
	regStamp time.Time
	cool     map[string]time.Time  // provider → cooling-down until (after a 429)
	dead     map[string]time.Time  // provider:model → quota-dead until (a hard 429: the key cannot use that model)
	cred     map[string]credResult // provider name → cached providers.CredentialPresent, TTL credTTL
	credGen  map[string]int        // provider name → generation, bumped on invalidate so a stale in-flight probe never resurrects it

	authState func(account string, now time.Time) (string, bool) // cached router.LoadState().AuthMarked lookup for attempts
	authAt    time.Time                                          // when authState was loaded
	marked    map[string]bool                                    // provider names this process marked auth-dead: cleared on their next success
	oauthTok  string                                             // cached vault CLAUDE_CODE_OAUTH_TOKEN (present results only), for the passthrough fallback
	oauthAt   time.Time                                          // oauthTok is valid until oauthAt+credTTL
	oauth     func() string                                      // reads the sealed subscription token; a seam for tests
	headers   map[string]string                                  // validated HARNESS_HEADERS, canonical names
	tags      string                                             // validated HARNESS_REQUEST_TAGS JSON, "" when unset or invalid
}

// credResult is one provider's cached CredentialPresent outcome.
type credResult struct {
	present bool
	until   time.Time
}

// quotaDeadFor is how long a hard-quota model is skipped before the router tries it again.
const quotaDeadFor = 30 * time.Minute

func newRouter(secret string) *Router {
	r := &Router{secret: secret, client: guardedClient(), log: os.Stderr, now: time.Now,
		cool: map[string]time.Time{}, dead: map[string]time.Time{}, cred: map[string]credResult{}, credGen: map[string]int{},
		marked: map[string]bool{}, oauth: providers.HeadlessOAuthToken}
	r.sessions = newSessionStore(func() time.Time { return r.now() })
	r.tags = parseRequestTags(os.Getenv(proxy.RequestTagsEnv), r.log)
	r.headers = proxy.ParseHeaders(os.Getenv(proxy.HeadersEnv), r.log)
	return r
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if !loopbackHost(req.Host) {
		writeError(w, http.StatusForbidden, "proxy: Host must be 127.0.0.1, localhost or [::1]")
		return
	}
	if req.URL.Path == "/healthz" {
		r.health(w)
		return
	}
	if req.URL.Path == proxy.SessionPath || req.URL.Path == proxy.SessionRevokePath {
		if !r.secretOK(req) { // a session token never mints or revokes
			writeError(w, http.StatusUnauthorized, "proxy: missing or wrong client secret")
			return
		}
		r.serveSession(w, req)
		return
	}
	if !r.authorized(req) {
		writeError(w, http.StatusUnauthorized, "proxy: missing or wrong client secret (apiKeyHelper: claude-code resolve --proxy)")
		return
	}
	r.inflight.Add(1)
	defer r.inflight.Add(-1)
	if !r.secretOK(req) { // a session token: pin its profile for the resolved target and fallbacks
		req = req.WithContext(context.WithValue(req.Context(), scopeKey{}, profileOfPath(req.URL.Path)))
	}
	// Past auth, Authorization survives only as the client's own credential (secret sent in proxy.ClientHeader).
	if req.Header.Get(proxy.ClientHeader) == "" {
		req.Header.Del("Authorization")
		req.Header.Del("X-Api-Key")
	}
	req.Header.Del(proxy.ClientHeader)
	if prov, dialect, rest, ok := parsePassthrough(req.URL.Path); ok {
		r.passthrough(w, req, prov, dialect, rest)
		return
	}
	session, count, ok := parsePath(req.URL.Path)
	if !ok || req.Method != http.MethodPost {
		writeError(w, http.StatusNotFound, "proxy: unsupported route "+req.Method+" "+req.URL.Path+" (want POST /p/<provider>/v1/messages)")
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
	auth   bool // the credential was condemned (login required or rejected after a re-mint)
}

func (e *fallbackError) Unwrap() error { return e.error }

// route resolves the target and walks the attempt ladder; it returns what the log line names.
func (r *Router) route(w *statusWriter, in *http.Request, session string, count bool) (string, string, int) {
	body, err := io.ReadAll(io.LimitReader(in.Body, maxBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "proxy: read body: "+err.Error())
		return session, "-", 0
	}
	if len(body) > maxBody {
		writeError(w, http.StatusRequestEntityTooLarge, "proxy: request body over 64MiB")
		return session, "-", 0
	}
	var head struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &head); err != nil || head.Model == "" {
		writeError(w, http.StatusBadRequest, "proxy: request body must be JSON with a model")
		return session, "-", 0
	}
	reg, err := r.registry()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "proxy: "+err.Error())
		return session, head.Model, 0
	}
	target, ok := resolveTarget(w, in, reg, session, head.Model)
	if !ok {
		return session, head.Model, 0
	}
	clientAuth := in.Header.Get("Authorization") != "" || r.oauthToken() != "" // the sealed token authenticates passthrough
	if msg := unroutable(target.Provider, clientAuth); msg != "" {
		writeError(w, http.StatusBadRequest, "proxy: "+msg)
		return target.Provider.Name, target.Model, 0
	}
	clientModel := models.Strip1M(head.Model)
	ladder := r.attempts(reg, target, clientAuth)
	ladder = confineLadder(in, ladder)
	return r.walkAttempts(w, in, body, ladder, clientModel, count)
}

func (r *Router) walkAttempts(w *statusWriter, in *http.Request, body []byte, ladder []providers.Target, clientModel string, count bool) (string, string, int) {
	var lastErr error
	for i := 0; i < len(ladder); i++ { // indexed: a hard-quota failure inserts the demoted model right after t
		t := ladder[i]
		last := i == len(ladder)-1
		done, err := r.try(w, in, body, t, clientModel, count, last)
		if done {
			r.clearOnSuccess(t.Provider.Name, w.status)
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
				fmt.Fprintf(r.log, "proxy: %s %s is quota-dead for this key, demoting to %s\n", t.Provider.Name, t.Model, d.Model)
				audit.Log(audit.RouterFallback, t.Provider.Name, "status=429 quota; demoted to "+d.Model)
				continue
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
	}
	return exhausted(w, ladder, lastErr)
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
	out = r.withoutAuthMarked(out)
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	cooling := func(t providers.Target) bool { return now.Before(r.cool[t.Provider.Name]) }
	sort.SliceStable(out, func(i, j int) bool { return !cooling(out[i]) && cooling(out[j]) })
	return out
}

// withoutAuthMarked drops targets whose provider has a live auth mark; when that empties the ladder it is kept whole.
func (r *Router) withoutAuthMarked(out []providers.Target) []providers.Target {
	marked, now := r.authMarks(), r.now()
	kept := make([]providers.Target, 0, len(out))
	for _, t := range out {
		if _, ok := marked(router.ProviderAccount(t.Provider.Name), now); !ok {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		return out
	}
	return kept
}

// authMarks is the route state, re-read from quota.json at most once per authStateTTL.
func (r *Router) authMarks() func(string, time.Time) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now := r.now(); r.authState == nil || now.Sub(r.authAt) >= authStateTTL {
		r.authState, r.authAt = router.LoadState().AuthMarked, now
	}
	return r.authState
}

// condemn records a provider as auth-dead in the route state; best-effort, the caller moves on regardless.
func (r *Router) condemn(p *providers.Provider, v providers.AuthVerdict) {
	if err := router.MarkAuth(router.ProviderAccount(p.Name), "auth "+string(v)+": "+authHint(p)); err != nil {
		fmt.Fprintf(r.log, "proxy: mark %s auth-dead: %v\n", p.Name, err)
		return
	}
	r.mu.Lock()
	r.marked[p.Name] = true
	r.authAt = time.Time{} // the next attempts() re-reads the state and sees the mark
	r.mu.Unlock()
}

// clearOnSuccess lifts a mark this process set once the provider answers with a 2xx.
func (r *Router) clearOnSuccess(name string, status int) {
	if status < 200 || status > 299 {
		return
	}
	r.mu.Lock()
	was := r.marked[name]
	delete(r.marked, name)
	r.mu.Unlock()
	if was {
		_ = router.ClearAuth(router.ProviderAccount(name)) // best-effort: the TTL backstop expires a missed clear
	}
}

// oauthToken is the vault's sealed subscription token, cached (present results only) for credTTL.
func (r *Router) oauthToken() string {
	r.mu.Lock()
	if now := r.now(); r.oauthTok != "" && now.Sub(r.oauthAt) < credTTL {
		tok := r.oauthTok
		r.mu.Unlock()
		return tok
	}
	r.mu.Unlock()
	tok := r.oauth()
	if tok != "" {
		r.mu.Lock()
		r.oauthTok, r.oauthAt = tok, r.now()
		r.mu.Unlock()
	}
	return tok
}

// demotion walks t's provider down the slot ladder to the first model not known to be quota-dead.
func (r *Router) demotion(t providers.Target) (providers.Target, bool) {
	seen := map[string]bool{t.Model: true} // a slot table like opus=A sonnet=B haiku=A demotes A→B→A forever
	for d, ok := providers.Demote(t.Provider, t.Model); ok && !seen[d.Model]; d, ok = providers.Demote(t.Provider, d.Model) {
		seen[d.Model] = true
		if !r.isDead(d) {
			return d, true
		}
	}
	return providers.Target{}, false
}

// credentialPresent caches only present credentials for credTTL, so newly added credentials are seen immediately.
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

// storeCredentialPresent records a successful probe only if no invalidation advanced its generation.
func (r *Router) storeCredentialPresent(name string, startGen int, until time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.credGen[name] != startGen {
		return
	}
	r.cred[name] = credResult{present: true, until: until}
}

// invalidateCredential clears cached presence and advances the generation to reject stale probes.
func (r *Router) invalidateCredential(name string) {
	r.mu.Lock()
	delete(r.cred, name)
	r.credGen[name]++
	r.mu.Unlock()
}

// deadKey is profile:canonical-model, so marked ([1m]) and unmarked spellings of one model share a verdict.
func deadKey(t providers.Target) string {
	id := models.Strip1M(t.Model)
	if c, _, ok := models.Resolve(id); ok {
		id = c
	}
	return t.Provider.Name + ":" + id
}

func (r *Router) isDead(t providers.Target) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now().Before(r.dead[deadKey(t)])
}

func (r *Router) markDead(t providers.Target) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dead[deadKey(t)] = r.now().Add(quotaDeadFor)
}

// try runs one attempt. done=false means nothing was written and err says why (the caller may fail over).
func (r *Router) try(w *statusWriter, in *http.Request, body []byte, t providers.Target, clientModel string, count, last bool) (bool, error) {
	if route, ok := providers.AnthropicRoute(t.Provider, t.Model); ok {
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
		var capture bytes.Buffer
		orig := resp.Body
		resp.Body = teeReadCloser{io.TeeReader(orig, &capture), orig}
		relay(w, resp)
		logLedgerUsage(in, p, t.Model, scrapeAnthropicUsage(capture.Bytes()))
		return true, nil
	}
	relay(w, resp)
	return true, nil
}

func (r *Router) tryOpenAI(w *statusWriter, in *http.Request, body []byte, t providers.Target, clientModel string, last bool) (bool, error) {
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
		eb := translate.FromChatError(resp.StatusCode, b)
		if eb == nil {
			eb = errorBody(resp.StatusCode, p.Name+": "+strings.TrimSpace(string(b)))
		}
		writeJSON(w, resp.StatusCode, eb)
		return true, nil
	}
	return r.relayChat(w, in, resp, t, clientModel, stream)
}

// send POSTs body with the provider's auth; a 401/403 refreshes the credential and replays once.
func (r *Router) send(in *http.Request, p *providers.Provider, url string, body []byte, hdr func(http.Header)) (*http.Response, error) {
	return r.sendMethod(in, http.MethodPost, p, url, body, hdr)
}

// sendMethod is send with an explicit HTTP method (GET passthrough carries no body).
func (r *Router) sendMethod(in *http.Request, method string, p *providers.Provider, url string, body []byte, hdr func(http.Header)) (*http.Response, error) {
	ctx := in.Context()
	do := func(credential string) (*http.Response, error) {
		return r.sendRequest(in, method, p, url, body, hdr, credential)
	}
	resp, err := do("")
	if err != nil {
		return nil, r.authFailure(p, err)
	}
	resp.Body = newIdleBody(resp.Body, idleTimeout) // before invalidToken's peek, so a stalled 400 body cannot hang
	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || invalidToken(resp, p)) &&
		p.Auth.HeaderStyle() != "" {
		drain(resp)
		r.invalidateCredential(p.Name) // invalidate before refreshing: a Refresh failure must not leave a stale "present" cached
		credential, err := providers.Refresh(ctx, p)
		if err != nil {
			return nil, r.authFailure(p, err)
		}
		if resp, err = do(credential); err != nil {
			return nil, err
		}
		resp.Body = newIdleBody(resp.Body, idleTimeout)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || invalidToken(resp, p) {
			drain(resp)
			r.condemn(p, providers.VerdictRejected)
			return nil, &fallbackError{error: errors.New("credential rejected after re-mint"), status: http.StatusUnauthorized, auth: true}
		}
	}
	return resp, nil
}
