// Package router is the loopback model router Claude Code talks to when a session needs translation,
// cross-provider slots, or failover.
package proxy

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/translate"
	"github.com/edotau/claude-code/internal/usageledger"
	"github.com/edotau/claude-code/internal/vault"
)

const (
	maxBody         = 64 << 20
	defaultCooldown = 30 * time.Second
	maxCooldown     = 5 * time.Minute
	credTTL         = 60 * time.Second   // providers.CredentialPresent cache: one keyring probe per provider per TTL
	authStateTTL    = 5 * time.Second    // router.LoadState cache: quota.json is read at most once per TTL, not per request
	oauthBeta       = "oauth-2025-04-20" // the anthropic-beta Claude Code sends with a subscription bearer token
)

// AuthGate is the route package's auth marks, injected because route imports router; the zero value is disabled.
type AuthGate struct {
	Account func(provider string) string                              // router.ProviderAccount
	Mark    func(account, reason string) error                        // router.MarkAuth
	Clear   func(account string) error                                // router.ClearAuth
	Marked  func() func(account string, now time.Time) (string, bool) // router.LoadState().AuthMarked, freshly loaded
}

// Gate is wired once by the serve path (internal/cli/gateway.go proxyServe).
var Gate AuthGate

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

	sessions *sessionStore // lifetime-limited per-profile tokens, memory only

	mu       sync.Mutex
	reg      *providers.Registry
	regStamp time.Time
	cool     map[string]time.Time  // provider → cooling-down until (after a 429)
	dead     map[string]time.Time  // provider:model → quota-dead until (a hard 429: the key cannot use that model)
	cred     map[string]credResult // provider name → cached providers.CredentialPresent, TTL credTTL
	credGen  map[string]int        // provider name → generation, bumped on invalidate so a stale in-flight probe never resurrects it

	authState func(account string, now time.Time) (string, bool) // cached Gate.Marked() lookup for attempts
	authAt    time.Time                                          // when authState was loaded
	marked    map[string]bool                                    // provider names this process marked auth-dead: cleared on their next success
	oauthTok  string                                             // cached vault CLAUDE_CODE_OAUTH_TOKEN (present results only), for the passthrough fallback
	oauthAt   time.Time                                          // oauthTok is valid until oauthAt+credTTL
	oauth     func() string                                      // reads the sealed subscription token; a seam for tests
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
	r.tags = parseRequestTags(os.Getenv(RequestTagsEnv), r.log)
	return r
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if !loopbackHost(req.Host) {
		writeError(w, http.StatusForbidden, "proxy: Host must be 127.0.0.1, localhost or [::1]")
		return
	}
	if req.URL.Path == "/healthz" {
		w.Header().Set(PIDHeader, strconv.Itoa(os.Getpid())) // lets a new router identify and replace this one
		_, _ = io.WriteString(w, "ok\n")
		return
	}
	if req.URL.Path == SessionPath || req.URL.Path == SessionRevokePath {
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
	if !r.secretOK(req) { // a session token: pin its profile for the resolved target and fallbacks
		req = req.WithContext(context.WithValue(req.Context(), scopeKey{}, profileOfPath(req.URL.Path)))
	}
	// Past auth, Authorization survives only as the client's own credential (secret sent in ClientHeader).
	if req.Header.Get(ClientHeader) == "" {
		req.Header.Del("Authorization")
		req.Header.Del("X-Api-Key")
	}
	req.Header.Del(ClientHeader)
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

// scopeKey carries the profile a session token is confined to; absent for the router secret.
type scopeKey struct{}

// sessionScope is the profile a session-token request is confined to.
func sessionScope(in *http.Request) (string, bool) {
	scope, ok := in.Context().Value(scopeKey{}).(string)
	return scope, ok
}

// resolveTarget maps the path profile and requested model to a target, writing the error when it cannot.
func resolveTarget(w http.ResponseWriter, in *http.Request, reg *providers.Registry, session, model string) (providers.Target, bool) {
	sess, err := reg.Get(session)
	if err != nil {
		writeError(w, http.StatusBadRequest, "proxy: "+err.Error())
		return providers.Target{}, false
	}
	target := providers.ParseTarget(reg, model, sess)
	return target, scopeAllows(w, in, target)
}

// scopeAllows rejects with 403 a resolved target outside a session token's profile.
func scopeAllows(w http.ResponseWriter, in *http.Request, t providers.Target) bool {
	scope, scoped := sessionScope(in)
	if scoped && t.Provider.Name != scope {
		writeError(w, http.StatusForbidden, "proxy: session token is limited to profile "+scope)
		return false
	}
	return true
}

// confineLadder drops fallback providers outside a session token's profile; they hold other credentials.
func confineLadder(in *http.Request, ladder []providers.Target) []providers.Target {
	scope, scoped := sessionScope(in)
	if !scoped {
		return ladder
	}
	return slices.DeleteFunc(ladder, func(t providers.Target) bool { return t.Provider.Name != scope })
}

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
	t := ladder[len(ladder)-1]
	var failure *fallbackError
	if errors.As(lastErr, &failure) && failure.auth {
		writeError(w, http.StatusUnauthorized, fmt.Sprintf("proxy: %s: authentication failed; %s", t.Provider.Name, authHint(t.Provider)))
		return t.Provider.Name, t.Model, len(ladder)
	}
	writeError(w, http.StatusBadGateway, fmt.Sprintf("proxy: %s: %v", t.Provider.Name, lastErr))
	return t.Provider.Name, t.Model, len(ladder)
}

// unroutable explains why the router cannot serve p at all, or returns "".
func unroutable(p *providers.Provider, clientAuth bool) string {
	if p.Auth.Login != "" {
		return fmt.Sprintf("provider %s is the %s sign-in; only claude-code run %s can use it", p.Name, p.Auth.Login, p.Auth.Login)
	}
	if p.Auth.Method() == providers.AuthCLI && !clientAuth {
		return fmt.Sprintf("provider %s uses passthrough auth but the request carries no client credential; launch it as the session provider (claude-code switch %s) so its login reaches the router", p.Name, p.Name)
	}
	_, a := p.Route(providers.ProtocolAnthropic)
	_, o := p.Route(providers.ProtocolOpenAI)
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
	if Gate.Marked == nil {
		return out
	}
	marked, now := r.authMarks(), r.now()
	kept := make([]providers.Target, 0, len(out))
	for _, t := range out {
		if _, ok := marked(Gate.Account(t.Provider.Name), now); !ok {
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
		r.authState, r.authAt = Gate.Marked(), now
	}
	return r.authState
}

// condemn records a provider as auth-dead in the route state; best-effort, the caller moves on regardless.
func (r *Router) condemn(p *providers.Provider, v providers.AuthVerdict) {
	if Gate.Mark == nil {
		return
	}
	if err := Gate.Mark(Gate.Account(p.Name), "auth "+string(v)+": "+authHint(p)); err != nil {
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
	if was && Gate.Clear != nil {
		_ = Gate.Clear(Gate.Account(name)) // best-effort: the TTL backstop expires a missed clear
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

// passthroughAuth sets the client's login on h, else the sealed subscription token (with its OAuth beta).
func (r *Router) passthroughAuth(h http.Header, in *http.Request, route string) {
	if a := in.Header.Get("Authorization"); a != "" {
		h.Set("Authorization", a)
		return
	}
	tok := r.oauthToken()
	if tok == "" || !anthropicOrigin(route) {
		return
	}
	h.Set("Authorization", "Bearer "+tok)
	if beta := h.Get("Anthropic-Beta"); !strings.Contains(beta, oauthBeta) {
		h.Set("Anthropic-Beta", strings.Trim(beta+","+oauthBeta, ","))
	}
}

// anthropicHost is the only host the sealed subscription token is sent to; a var so tests can point it at httptest.
var anthropicHost = "api.anthropic.com"

// anthropicOrigin reports whether route is https on anthropicHost exactly (no suffix or userinfo tricks).
func anthropicOrigin(route string) bool {
	u, err := url.Parse(route)
	return err == nil && u.Scheme == "https" && u.User == nil && u.Host == anthropicHost
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

// credentialPresent caches providers.CredentialPresent per provider name for credTTL, present results
// only: an env-auth provider with no process env var falls through to a vault read (a bbolt
// open per call), and a present credential is probed once per TTL instead of every routed request.
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

// teeReadCloser lets a response body be read through a tee without losing its own Close.
type teeReadCloser struct {
	io.Reader
	io.Closer
}

// ledgerUsage is the token counts scraped from a provider's raw bytes for the usage ledger.
type ledgerUsage struct{ input, output, cacheRead int }

// usageBlockRE matches one `"usage":{...}` object with at most one nested level (prompt_tokens_details).
var usageBlockRE = regexp.MustCompile(`"usage"\s*:\s*\{(?:[^{}]|\{[^{}]*\})*\}`)

// wrapUsage turns a matched `"usage":{...}` pair into a standalone JSON object.
func wrapUsage(m []byte) []byte {
	_, v, _ := bytes.Cut(m, []byte(":"))
	return v
}

// scrapeChatUsage reads the last OpenAI-dialect usage object; input excludes cached tokens, as translate does.
func scrapeChatUsage(body []byte) ledgerUsage {
	matches := usageBlockRE.FindAll(body, -1)
	if len(matches) == 0 {
		return ledgerUsage{}
	}
	var raw struct {
		Prompt     int `json:"prompt_tokens"`
		Completion int `json:"completion_tokens"`
		Details    *struct {
			Cached int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	}
	if json.Unmarshal(wrapUsage(matches[len(matches)-1]), &raw) != nil {
		return ledgerUsage{}
	}
	u := ledgerUsage{input: raw.Prompt, output: raw.Completion}
	if raw.Details != nil && raw.Details.Cached > 0 && raw.Details.Cached <= u.input {
		u.cacheRead = raw.Details.Cached
		u.input -= u.cacheRead
	}
	return u
}

// scrapeAnthropicUsage sums every usage object: a stream splits input (message_start) from output (message_delta).
func scrapeAnthropicUsage(body []byte) ledgerUsage {
	var u ledgerUsage
	for _, m := range usageBlockRE.FindAll(body, -1) {
		var raw struct {
			Input     int `json:"input_tokens"`
			Output    int `json:"output_tokens"`
			CacheRead int `json:"cache_read_input_tokens"`
		}
		if json.Unmarshal(wrapUsage(m), &raw) == nil {
			u.input += raw.Input
			u.output += raw.Output
			u.cacheRead += raw.CacheRead
		}
	}
	return u
}

// metered is every row the proxy bills per token; a cli passthrough rides the client's subscription and bills nothing.
func metered(p *providers.Provider) bool { return p.Auth.Method() != providers.AuthCLI }

// Optional launcher-set request headers that attribute a ledger entry; absent today, so those fields stay empty.
const (
	AgentHeader   = "X-Claude-Code-Agent"
	SessionHeader = "X-Claude-Code-Session"
)

// logLedgerUsage records one metered request to the usage ledger; best-effort, zero counts skipped.
func logLedgerUsage(in *http.Request, p *providers.Provider, model string, u ledgerUsage) {
	if u.input == 0 && u.output == 0 && u.cacheRead == 0 {
		return
	}
	session := in.Header.Get(SessionHeader)
	if session == "" {
		session = in.Header.Get("X-Claude-Code-Session-Id")
	}
	usageledger.Append(usageledger.Entry{
		Ts: time.Now(), Profile: p.Name, Provider: p.Provider, Model: model,
		Agent: in.Header.Get(AgentHeader), Session: session,
		Input: u.input, Output: u.output, CacheRead: u.cacheRead,
	})
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
	// Per-token providers tee raw bytes so the ledger can scrape usage without changing translate's API.
	var capture *bytes.Buffer
	body2 := io.Reader(resp.Body)
	if metered(p) {
		capture = &bytes.Buffer{}
		body2 = io.TeeReader(resp.Body, capture)
	}
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		// No explicit 200: a read error before the first event writes nothing, so the caller can still fail over.
		err := translate.StreamChat(w, flusher(w), body2, clientModel)
		if capture != nil {
			logLedgerUsage(in, p, t.Model, scrapeChatUsage(capture.Bytes()))
		}
		if err != nil && w.status == 0 {
			return false, err
		}
		if err != nil && in.Context().Err() == nil {
			fmt.Fprintf(r.log, "proxy: %s stream translation: %v\n", p.Name, err)
		}
		return true, nil
	}
	b, err := io.ReadAll(io.LimitReader(body2, maxBody))
	if err != nil {
		return false, err
	}
	if capture != nil {
		logLedgerUsage(in, p, t.Model, scrapeChatUsage(capture.Bytes()))
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
	return r.sendMethod(in, http.MethodPost, p, url, body, hdr)
}

// sendMethod is send with an explicit HTTP method (GET passthrough carries no body).
func (r *Router) sendMethod(in *http.Request, method string, p *providers.Provider, url string, body []byte, hdr func(http.Header)) (*http.Response, error) {
	ctx := in.Context()
	do := func(credential string) (*http.Response, error) {
		if n, ok := ctx.Value(attemptsKey{}).(*int); ok {
			*n++
		}
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		hdr(req.Header)
		r.attribute(req.Header, in, p)
		if credential == "" {
			if err := providers.Authorize(ctx, p, req.Header); err != nil {
				return nil, err
			}
		} else {
			for k, v := range p.Headers {
				req.Header.Set(k, v)
			}
			providers.SetCredential(p, req.Header, credential)
		}
		resp, err := r.client.Do(req)
		if err != nil {
			return resp, &fallbackError{error: err}
		}
		return resp, nil
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
		if resp.StatusCode == http.StatusUnauthorized || invalidToken(resp, p) {
			drain(resp)
			r.condemn(p, providers.VerdictRejected)
			return nil, &fallbackError{error: errors.New("credential rejected after re-mint"), status: http.StatusUnauthorized, auth: true}
		}
	}
	return resp, nil
}

// authFailure condemns p when err's verdict says only a login can fix it (the router then moves on); else err as is.
func (r *Router) authFailure(p *providers.Provider, err error) error {
	v := providers.Verdict(err)
	if !v.Condemns() {
		return err
	}
	r.condemn(p, v)
	return &fallbackError{error: errors.New("authentication failed (" + string(v) + ")"), status: http.StatusUnauthorized, auth: true}
}

// invalidToken is the AI Gateway's 400 "Invalid Token" on p; it peeks 256 bytes and leaves the body readable.
func invalidToken(resp *http.Response, p *providers.Provider) bool {
	if resp.StatusCode != http.StatusBadRequest {
		return false
	}
	peek, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(peek), resp.Body), resp.Body}
	return providers.IsInvalidTokenResponse(p, resp.StatusCode, peek)
}

// failover classifies a failed response: a hard-quota 429 marks the model dead (and moves on even on the last
// attempt when the provider has a lower slot); a transient 429 cools the provider; retryable statuses move on
// unless this was the last attempt. moveOn means the response was drained and the caller may try the next target.
func (r *Router) failover(resp *http.Response, t providers.Target, last bool) (moveOn, hard bool) {
	p := t.Provider
	if tooLong(resp) {
		return false, false
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		peek, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(peek), resp.Body), resp.Body}
		if hard = providers.ModelDenied(string(peek)); hard {
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

// promptTooLong matches the upstream limit text, e.g. "prompt is too long: 1177204 tokens > 1000000 maximum".
var promptTooLong = regexp.MustCompile(`prompt is too long(: \d+ tokens > \d+ maximum)?`)

// tooLong rewrites a 400 "prompt is too long" into the router's invalid_request_error, quoting only the limit text.
func tooLong(resp *http.Response) bool {
	if resp.StatusCode != http.StatusBadRequest {
		return false
	}
	peek, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(peek), resp.Body), resp.Body}
	m := promptTooLong.Find(peek)
	if m == nil {
		return false
	}
	drain(resp)
	body := errorBody(http.StatusBadRequest, string(m))
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header = http.Header{"Content-Type": {"application/json"}}
	return true
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

// registry reloads on vault mtime changes, keeping the last good table on a bad overlay or a busy vault.
func (r *Router) registry() (*providers.Registry, error) {
	var stamp time.Time
	if fi, err := os.Stat(vault.Path()); err == nil {
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
		// regStamp stays behind so the next call retries providers.Load instead of caching the failure.
		fmt.Fprintf(r.log, "proxy: keeping previous registry: %v\n", err)
		return r.reg, nil
	}
	r.reg, r.regStamp = reg, stamp
	return reg, nil
}

func presentedSecret(req *http.Request) string {
	got := req.Header.Get(ClientHeader)
	if got == "" {
		got = req.Header.Get("X-Api-Key")
	}
	if got == "" {
		got, _ = strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	}
	return got
}

func (r *Router) secretOK(req *http.Request) bool {
	got := presentedSecret(req)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(r.secret)) == 1
}

// authorized accepts the router secret anywhere, or a session token on its own profile's /p/ routes.
func (r *Router) authorized(req *http.Request) bool {
	if r.secretOK(req) {
		return true
	}
	return r.sessions.accept(presentedSecret(req), profileOfPath(req.URL.Path))
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
