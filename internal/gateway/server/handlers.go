package server

import (
	"bytes"
	"cmp"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/gateway/translate"
	"github.com/edotau/claude-code/internal/network/egress"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// retryable statuses move to the next attempt when nothing has been written to the client yet.
var retryable = map[int]bool{429: true, 500: true, 502: true, 503: true, 504: true, 529: true}

var loadRegistry = providers.Load

// authFailure condemns p when err's verdict says only a login can fix it (the router then moves on); else err as is.
func (r *Router) authFailure(p *providers.Provider, err error) error {
	v := providers.Verdict(err)
	if !v.Condemns() {
		return err
	}
	r.condemn(p, v)
	return &fallbackError{
		error:  errors.New("authentication failed (" + string(v) + ")"),
		status: http.StatusUnauthorized,
		auth:   true,
	}
}

// readBody reads the request body up to maxBody; a read error or an oversize body is written out and returns false.
func readBody(w http.ResponseWriter, in *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(in.Body, maxBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "proxy: read body: "+err.Error())
		return nil, false
	}
	if len(body) > maxBody {
		writeError(w, http.StatusRequestEntityTooLarge, "proxy: request body over 64MiB")
		return nil, false
	}
	return body, true
}

// invalidToken is the AI Gateway's 400 "Invalid Token" on p; it peeks 256 bytes and leaves the body readable.
func invalidToken(resp *http.Response, p *providers.Provider) bool {
	if resp.StatusCode != http.StatusBadRequest {
		return false
	}
	peek := peekBody(resp, 256)
	return providers.IsInvalidTokenResponse(p, resp.StatusCode, peek)
}

// peekBody reads up to n bytes of resp.Body and leaves the whole body readable again.
func peekBody(resp *http.Response, n int64) []byte {
	peek, _ := io.ReadAll(io.LimitReader(resp.Body, n))
	resp.Body = teeReadCloser{io.MultiReader(bytes.NewReader(peek), resp.Body), resp.Body}
	return peek
}

// credentialMarkers are lower-cased 403 body fragments naming a refused credential, not a policy or WAF block.
var credentialMarkers = []string{"invalid_token", "invalid_grant", "invalid_client", "invalid access token",
	"invalid token", "expired_token", "expired token", "token expired", "token has expired", "token is expired"}

// rejectedAfterRemint reports a 401, a 400 Invalid Token, or a 403 whose body or WWW-Authenticate names the credential.
func rejectedAfterRemint(resp *http.Response, p *providers.Provider) bool {
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return true
	case http.StatusForbidden:
		if strings.Contains(strings.ToLower(resp.Header.Get("WWW-Authenticate")), "invalid_token") {
			return true
		}
		peek := peekBody(resp, 4<<10)
		low := strings.ToLower(string(peek))
		return slices.ContainsFunc(credentialMarkers, func(m string) bool { return strings.Contains(low, m) })
	}
	return invalidToken(resp, p)
}

// failover marks denied models dead, cools rate-limited providers, and drains responses when moving on.
func (r *Router) failover(resp *http.Response, t providers.Target, last bool) (moveOn, hard bool) {
	p := t.Provider
	if tooLong(resp) {
		return false, false
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		peek := peekBody(resp, 64<<10)
		if hard = providers.ModelDenied(string(peek)); hard {
			r.markDead(t)
			if _, ok := r.demotion(t); ok {
				drain(resp)
				return true, true
			}
		} else {
			r.coolDown(p, resp)
		}
	}
	if last || !retryable[resp.StatusCode] {
		return false, hard
	}
	drain(resp)
	return true, hard
}

// coolDown sinks p for resp's Retry-After (clamped) after a transient 429; a hold of coolPersistMin+ is also persisted.
func (r *Router) coolDown(p *providers.Provider, resp *http.Response) {
	now := r.now()
	d := retryAfter(resp.Header.Get("Retry-After"), now)
	until := now.Add(d)
	r.mu.Lock()
	r.cool[p.Name] = until
	r.mu.Unlock()
	if d < coolPersistMin {
		return
	}
	r.bg.Go(func() { // off the request path; a failed write is only logged
		ran, err := router.MarkCooling(router.AccountOf(p), until, now)
		if err != nil || !ran {
			fmt.Fprintf(r.log, "proxy: persist %s cooling: %v\n", p.Name, cmp.Or(err, errors.New("state lock busy")))
		}
	})
}

// promptTooLong matches the upstream limit text, e.g. "prompt is too long: 1177204 tokens > 1000000 maximum".
var promptTooLong = regexp.MustCompile(`prompt is too long(: \d+ tokens > \d+ maximum)?`)

// tooLong rewrites a 400 "prompt is too long" into the router's invalid_request_error, quoting only the limit text.
func tooLong(resp *http.Response) bool {
	if resp.StatusCode != http.StatusBadRequest {
		return false
	}
	peek := peekBody(resp, 512)
	m := promptTooLong.Find(peek)
	if m == nil {
		return false
	}
	drain(resp)
	body := translate.ErrorBody(http.StatusBadRequest, string(m))
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header = http.Header{"Content-Type": {"application/json"}}
	return true
}

func retryAfter(v string, now time.Time) time.Duration {
	d := defaultCooldown
	if s, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && s > 0 {
		d = time.Duration(
			min(s, int(maxCooldown/time.Second)),
		) * time.Second // clamp first: a huge value would overflow
	} else if at, err := http.ParseTime(v); err == nil && at.After(now) {
		d = at.Sub(now)
	}
	return min(d, maxCooldown)
}

// registryRetry spaces vault reloads after a failed load.
const registryRetry = 5 * time.Second

// registry reloads on vault mtime changes, keeping the last good table on a bad overlay or a busy vault.
func (r *Router) registry() (*providers.Registry, error) {
	var stamp time.Time
	if fi, err := os.Stat(vault.Path()); err == nil {
		stamp = fi.ModTime()
	}
	if reg, ok := r.cachedRegistry(stamp); ok {
		return reg, nil
	}
	r.regLoad.Lock() // one load at a time, without r.mu: a slow vault decrypt must not stall every request
	defer r.regLoad.Unlock()
	if reg, ok := r.cachedRegistry(stamp); ok { // another caller loaded it while this one waited
		return reg, nil
	}
	if prev, backoff := r.registryBackoff(); backoff {
		return prev, nil
	}
	reg, err := loadRegistry()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		if r.reg == nil {
			return nil, err
		}
		// regStamp stays behind and retries wait out registryRetry, so a bad overlay is neither cached nor hammered.
		r.regRetry = r.now().Add(registryRetry)
		fmt.Fprintf(r.log, "proxy: keeping previous registry (retry in %s): %v\n", registryRetry, err)
		return r.reg, nil
	}
	r.reg, r.regStamp, r.regRetry = reg, stamp, time.Time{}
	return reg, nil
}

// registryBackoff returns the previous registry while a failed load's retry window is still open.
func (r *Router) registryBackoff() (*providers.Registry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reg, r.reg != nil && r.now().Before(r.regRetry)
}

// cachedRegistry returns the loaded table when it matches the vault stamp.
func (r *Router) cachedRegistry(stamp time.Time) (*providers.Registry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reg, r.reg != nil && stamp.Equal(r.regStamp)
}

func presentedSecret(req *http.Request) string {
	got := req.Header.Get(gateway.ClientHeader)
	if got == "" {
		got = req.Header.Get("X-Api-Key")
	}
	if got == "" {
		if tok, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer "); ok {
			got = tok
		}
	}
	return got
}

func (r *Router) secretOK(req *http.Request) bool {
	got := presentedSecret(req)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(r.secret)) == 1
}

// authorized accepts the router secret anywhere, a session token on its profile's /p/ routes, or a client token.
func (r *Router) authorized(req *http.Request) bool {
	if r.secretOK(req) {
		return true
	}
	tok, profile := presentedSecret(req), profileOfPath(req.URL.Path)
	return r.sessions.accept(tok, profile) || r.clientAccepted(req, tok, profile)
}

// admit scopes a token to its profile set, drops the proxy auth headers, and honours the identity for launches only.
func (r *Router) admit(req *http.Request) *http.Request {
	launch := true
	if !r.secretOK(req) { // a session or client token: pin its profiles for the resolved target and fallbacks
		tok := presentedSecret(req)
		launch = !strings.HasPrefix(tok, clientTokenPf) // an editor's client token picks no identity
		scope := r.sessions.profiles(tok)
		if scope == nil { // a client token reaches only its path's profile
			scope = []string{profileOfPath(req.URL.Path)}
		}
		req = req.WithContext(context.WithValue(req.Context(), scopeKey{}, scope))
	}
	stripClientAuth(req)
	if launch {
		req = withLaunchIdentity(req)
	}
	return req
}

// refuseUnauthorized answers a request no credential admits: 403 for a client token out of scope, else 401.
func (r *Router) refuseUnauthorized(w http.ResponseWriter, req *http.Request) {
	if !r.clients.valid(presentedSecret(req)) {
		r.refuse(w, req, http.StatusUnauthorized,
			"proxy: missing or wrong client secret (apiKeyHelper: claude-code resolve --proxy)")
		return
	}
	if _, err := r.registry(); err != nil { // the scope check could not run: not the token's fault
		r.refuse(w, req, http.StatusServiceUnavailable, "proxy: provider registry unavailable: "+err.Error())
		return
	}
	r.refuse(w, req, http.StatusForbidden, "proxy: a client token reaches only /p/<Databricks serving profile>/ routes")
}

// clientAccepted reports a valid client token on a Databricks serving row's /p/<profile>/ route, never under /via/.
func (r *Router) clientAccepted(req *http.Request, tok, profile string) bool {
	if egress.PinnedHost(req.Context()) != "" || !validProfile(profile) || !r.clients.valid(tok) {
		return false
	}
	reg, err := r.registry()
	return err == nil && reg.ServingRow(profile) == nil
}

func loopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "::1" || strings.EqualFold(host, "localhost")
}

// unroutable explains why the router cannot serve p at all, or returns "".
func unroutable(p *providers.Provider, clientAuth bool) string {
	if p.Auth.Login != "" {
		return fmt.Sprintf("provider %s is the %s sign-in; only claude-code %s --profile %s can use it", p.Name,
			p.Auth.Login, p.Auth.Login, p.Name)
	}
	if p.Auth.Method() == providers.AuthCLI && !clientAuth {
		return fmt.Sprintf(
			"provider %s uses passthrough auth but the request carries no client credential; "+
				"launch it as the session provider (claude-code switch %s) so its login reaches the router",
			p.Name,
			p.Name,
		)
	}
	_, a := p.Route(providers.ProtocolAnthropic)
	_, o := p.Route(providers.ProtocolOpenAI)
	if !a && !o {
		return fmt.Sprintf("provider %s has no anthropic or openai route (dialects: %s)", p.Name,
			strings.Join(p.Dialects(), ","))
	}
	return ""
}

// authHint names the remedy a rejected credential needs, picked by the provider's auth type.
func authHint(p *providers.Provider) string {
	a := p.Auth
	switch {
	// login refuses a pat row without the switch
	case p.Preset == providers.PresetDatabricks && a.Type == providers.AuthPAT:
		return "run `claude-code databricks login --profile " + p.Name + " --auth-type oauth`"
	case p.Preset == providers.PresetDatabricks || a.Type == providers.AuthOAuth || a.Type == providers.AuthM2M:
		return "run `claude-code databricks login --profile " + p.Name + "`"
	case a.Command != "":
		return "check the provider's auth.command"
	case (a.Type == providers.AuthKey || a.Type == providers.AuthPAT) && a.Env != "":
		return "run `claude-code vault set " + a.Env + "`"
	}
	return "run `claude-code configure provider --list " + p.Name + "`"
}

func (r *Router) health(w http.ResponseWriter) {
	w.Header().Set(gateway.PIDHeader, strconv.Itoa(os.Getpid())) // lets a new router identify and replace this one
	w.Header().Set(gateway.HeaderBuild, textutil.BuildVersion())
	w.Header().Set(gateway.HeaderConfig, gateway.ConfigID())
	w.Header().Set(gateway.HeaderSessions, strconv.Itoa(r.sessions.count()))
	w.Header().Set(gateway.HeaderInflight, strconv.FormatInt(r.inflight.Load(), 10))
	_, _ = io.WriteString(w, "ok\n")
}

func exhausted(w http.ResponseWriter, ladder []providers.Target, lastErr error) (string, string, int) {
	if len(ladder) == 0 { // confineLadder can empty the ladder when no fallback is in the session's profile
		writeError(w, http.StatusForbidden, "proxy: no provider in this session's scope is usable")
		return "", "", 0
	}
	t := ladder[len(ladder)-1]
	writeSendError(w, t.Provider, lastErr)
	return t.Provider.Name, t.Model, len(ladder)
}
