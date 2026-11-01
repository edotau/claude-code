package server

import (
	"bytes"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/buildver"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/vault"
)

// retryable statuses move to the next attempt when nothing has been written to the client yet.
var retryable = map[int]bool{429: true, 500: true, 502: true, 503: true, 504: true, 529: true}

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

// failover marks denied models dead, cools rate-limited providers, and drains responses when moving on.
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
	got := req.Header.Get(proxy.ClientHeader)
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

// authHint names the remedy a rejected credential needs, picked by the provider's auth type.
func authHint(p *providers.Provider) string {
	a := p.Auth
	switch {
	case p.Preset == providers.PresetDatabricks && a.Type == providers.AuthPAT: // login refuses a pat row without the switch
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
	w.Header().Set(proxy.PIDHeader, strconv.Itoa(os.Getpid())) // lets a new router identify and replace this one
	w.Header().Set(proxy.HeaderBuild, buildver.Version())
	w.Header().Set(proxy.HeaderConfig, proxy.ConfigID())
	w.Header().Set(proxy.HeaderSessions, strconv.Itoa(r.sessions.count()))
	w.Header().Set(proxy.HeaderInflight, strconv.FormatInt(r.inflight.Load(), 10))
	_, _ = io.WriteString(w, "ok\n")
}

func exhausted(w http.ResponseWriter, ladder []providers.Target, lastErr error) (string, string, int) {
	t := ladder[len(ladder)-1]
	var failure *fallbackError
	if errors.As(lastErr, &failure) && failure.auth {
		writeError(w, http.StatusUnauthorized, fmt.Sprintf("proxy: %s: authentication failed; %s", t.Provider.Name, authHint(t.Provider)))
		return t.Provider.Name, t.Model, len(ladder)
	}
	writeError(w, http.StatusBadGateway, fmt.Sprintf("proxy: %s: %v", t.Provider.Name, lastErr))
	return t.Provider.Name, t.Model, len(ladder)
}
