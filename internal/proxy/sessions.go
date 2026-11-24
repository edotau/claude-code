package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/httpx"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

type SessionRequest struct {
	Profiles         []string `json:"profiles,omitempty"`
	PID              int      `json:"pid,omitempty"`
	FailoverProvider string   `json:"failover_provider,omitempty"`
	Fence            []string `json:"fence,omitempty"`
	Open             bool     `json:"open,omitempty"`
}

type fenceKey struct{}

type fenceOverride struct {
	fence []string
	open  bool
}

// WithFence overrides the fence a mint under ctx carries; only verified token claims may feed it.
func WithFence(ctx context.Context, fence []string, open bool) context.Context {
	return context.WithValue(ctx, fenceKey{}, fenceOverride{fence: fence, open: open})
}

// ArmAllows reports whether a databricks:<arm> confinement admits row p: an own sign-in, never a Databricks row.
func ArmAllows(p *providers.Provider) bool {
	return p != nil && p.Preset != providers.PresetDatabricks &&
		(p.Auth.Login != "" || p.Auth.Method() == providers.AuthCLI)
}

// ConfinementFence is the profile set the overlay confines ctx to; open when unconfined, an error when malformed.
func ConfinementFence(ctx context.Context) (fence []string, open bool, err error) {
	if o, ok := ctx.Value(fenceKey{}).(fenceOverride); ok {
		return o.fence, o.open, nil
	}
	if _, _, err := router.OverlayRoute(ctx); err != nil {
		return nil, false, fmt.Errorf("confinement overlay: %w", err)
	}
	arm := router.ArmProfileContext(ctx)
	if arm == "" {
		return nil, true, nil
	}
	reg, err := providers.LoadShared()
	if err != nil {
		return nil, false, fmt.Errorf("confinement fence: %w", err)
	}
	fence = []string{arm}
	for _, n := range reg.Names() {
		if n != arm && ArmAllows(reg.Row(n)) {
			fence = append(fence, n)
		}
	}
	slices.Sort(fence)
	return fence, false, nil
}

type SessionResponse struct {
	Token     string `json:"token"`
	ExpiresIn int    `json:"expires_in"`
}

// MintScopedToken asks the daemon (starting it if needed) for a token covering profiles and bound to pid.
func MintScopedToken(ctx context.Context, profiles []string, pid int) (string, error) {
	return MintScopedTokenWithFailover(ctx, profiles, pid, "")
}

// MintScopedTokenWithFailover mints a profile-bound token with optional server-side same-provider failover.
func MintScopedTokenWithFailover(ctx context.Context, profiles []string, pid int, failoverProvider string) (string, error) {
	fence, open, err := ConfinementFence(ctx)
	if err != nil {
		return "", err
	}
	var out SessionResponse
	req := SessionRequest{Profiles: profiles, PID: pid, FailoverProvider: failoverProvider, Fence: fence, Open: open}
	if err := postSession(ctx, SessionPath, req, &out); err != nil {
		return "", err
	}
	// an older daemon ignores the fence fields and signs a fenceless token: never hand that back as fenced
	if c, ok := PeekSessionClaims(out.Token); !ok || c.Open != open || !slices.Equal(c.Fence, fence) {
		return "", errors.New("proxy: the daemon signed a token without the requested confinement fence; restart the proxy")
	}
	return out.Token, nil
}

func postSession(ctx context.Context, path string, body SessionRequest, out *SessionResponse) error {
	if _, err := Ensure(ctx); err != nil {
		return err
	}
	st, err := readLiveState()
	if err != nil {
		return err
	}
	return postWithState(ctx, st, path, body, out)
}

// readLiveState returns the recorded daemon state, ErrNotRunning when absent or its pid is dead.
func readLiveState() (State, error) {
	st, err := ReadState()
	if errors.Is(err, os.ErrNotExist) {
		return st, ErrNotRunning
	}
	if err != nil {
		return st, fmt.Errorf("proxy: read state: %w", err)
	}
	if !alive(st.PID) {
		return st, ErrNotRunning
	}
	return st, nil
}

// postWithState verifies st's holder, then POSTs body with the router secret to the daemon.
func postWithState(ctx context.Context, st State, path string, body SessionRequest, out *SessionResponse) error {
	ctx, cancel := context.WithTimeout(ctx, sessionTimeout)
	defer cancel()
	if err := verifyHolder(ctx, st); err != nil {
		return err
	}
	secret, err := ClientSecret()
	if err != nil {
		return err
	}
	return sendSession(ctx, Base(st.Port)+path, path, secret, body, out)
}

// sessionClient never follows a redirect, so the router secret stays on the loopback proxy.
var sessionClient = &http.Client{CheckRedirect: httpx.SameOriginRedirect}

// sendSession POSTs body with the router secret to url; a non-2xx answer is an error naming path.
func sendSession(ctx context.Context, url, path, secret string, body SessionRequest, out *SessionResponse) error {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := sessionClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("proxy %s: %s", path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// verifyHolder confirms the /healthz answer on st.Port comes from st.PID before the router secret is sent there.
func verifyHolder(ctx context.Context, st State) error {
	resp, err := healthGet(ctx, st.Port)
	if errors.Is(err, syscall.ECONNREFUSED) { // nothing listens: the recorded pid is not a live proxy
		return ErrNotRunning
	}
	if err != nil {
		return fmt.Errorf("proxy pid %d unreachable on port %d: %w", st.PID, st.Port, err)
	}
	_ = resp.Body.Close()
	if got := resp.Header.Get(PIDHeader); resp.StatusCode != http.StatusOK || got != strconv.Itoa(st.PID) {
		return fmt.Errorf("port %d answers %s as pid %q, not proxy pid %d; secret withheld", st.Port, resp.Status, got,
			st.PID)
	}
	return nil
}

// sessionTimeout bounds the POST so a wedged daemon cannot hang the caller.
const sessionTimeout = 5 * time.Second

// SessionPath is the POST mint endpoint, requiring the router secret.
const SessionPath = "/internal/session"

// proxyTokenMaxAge re-mints inside the router's 7-day idle lifetime, as agent.SessionToken does.
const proxyTokenMaxAge = time.Hour

// mintScoped is the mint seam of ResolveProxyToken.
var mintScoped = MintScopedToken

// proxyTokenStart errors on native Windows, so proxy session tokens are unsupported there by decision.
var proxyTokenStart = proc.StartTime

// mintScopedFailover is the mint seam of a token carrying a same-provider failover group.
var mintScopedFailover = MintScopedTokenWithFailover

type proxyTokenRecord struct {
	Token    string    `json:"token"`
	Start    string    `json:"start"`
	Scope    []string  `json:"scope"`
	Failover string    `json:"failover,omitempty"`
	Fence    []string  `json:"fence,omitempty"`
	Open     bool      `json:"open,omitempty"`
	MintedAt time.Time `json:"minted_at"`
}

func proxyTokenDir() string { return filepath.Join(paths.StateDir(), "proxy-tokens") }

// ResolveProxyToken caches pid's scoped token (0600) while fresh with matching process identity and scope.
func ResolveProxyToken(ctx context.Context, scope []string, pid int) (string, error) {
	return resolveProxyToken(ctx, scope, pid, "", false)
}

// ResolveProxyTokenWithFailover is ResolveProxyToken for a token that also carries failover's group.
func ResolveProxyTokenWithFailover(ctx context.Context, scope []string, pid int, failover string) (string, error) {
	return resolveProxyToken(ctx, scope, pid, failover, false)
}

// RefreshProxyToken discards pid's cached token and mints and caches a new one; the old ends with pid like it.
func RefreshProxyToken(ctx context.Context, scope []string, pid int) (string, error) {
	return resolveProxyToken(ctx, scope, pid, "", true)
}

func resolveProxyToken(ctx context.Context, scope []string, pid int, failover string, refresh bool) (string, error) {
	fence, open, err := ConfinementFence(ctx)
	if err != nil {
		return "", err
	}
	ctx = WithFence(ctx, fence, open) // one snapshot for the cache binding and the mint
	start, err := proxyTokenStart(pid)
	if err != nil {
		return "", fmt.Errorf("identify token process %d: %w", pid, err)
	}
	dir := proxyTokenDir()
	pruneProxyTokens(dir)
	file := filepath.Join(dir, strconv.Itoa(pid)+".json")
	if !refresh {
		want := proxyTokenRecord{Start: start, Scope: scope, Failover: failover, Fence: fence, Open: open}
		if tok, ok := cachedProxyToken(file, want); ok {
			return tok, nil
		}
	}
	tok, err := mintFor(ctx, scope, pid, failover)
	if err != nil {
		return "", fmt.Errorf("mint scoped token for profiles %s: %w", strings.Join(scope, ","), err)
	}
	b, err := json.Marshal(proxyTokenRecord{Token: tok, Start: start, Scope: scope, Failover: failover, Fence: fence, Open: open,
		MintedAt: time.Now()})
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := paths.AtomicWrite(file, b, 0o600); err != nil {
		return "", fmt.Errorf("cache scoped token: %w", err)
	}
	return tok, nil
}

func mintFor(ctx context.Context, scope []string, pid int, failover string) (string, error) {
	if failover != "" {
		return mintScopedFailover(ctx, scope, pid, failover)
	}
	return mintScoped(ctx, scope, pid)
}

// cachedProxyToken reads file's token when its process identity and scope match and it is still fresh.
func cachedProxyToken(file string, want proxyTokenRecord) (string, bool) {
	b, err := os.ReadFile(file)
	if err != nil {
		return "", false
	}
	var rec proxyTokenRecord
	if json.Unmarshal(b, &rec) != nil || rec.Token == "" || !rec.sameBinding(want) ||
		time.Since(rec.MintedAt) >= proxyTokenMaxAge || SessionRefused(SessionInvalid(rec.Token, "")) {
		return "", false
	}
	return rec.Token, true
}

// sameBinding reports r was minted for want's process, scope, failover group and confinement fence.
func (r proxyTokenRecord) sameBinding(want proxyTokenRecord) bool {
	return r.Start == want.Start && slices.Equal(r.Scope, want.Scope) && r.Failover == want.Failover &&
		slices.Equal(r.Fence, want.Fence) && r.Open == want.Open
}

// ErrSessionUnverified means only a proxy can judge the token: the secret is unreadable here, or it is a legacy token.
var ErrSessionUnverified = errors.New("session token cannot be verified here (router secret unreadable or a legacy token)")

// SessionRefused reports a SessionInvalid result that is a definite refusal, not ErrSessionUnverified.
func SessionRefused(err error) bool { return err != nil && !errors.Is(err, ErrSessionUnverified) }

// SessionInvalid is why this config's proxies would refuse tok on profile ("" = any) now; nil when they accept it.
func SessionInvalid(tok, profile string) error {
	secret, ok, err := vaultSecret() // read only: a check never mints the router secret
	// a stored previous-build token lives in the proxy's bridge
	if err != nil || !ok || LegacySessionShape(tok) {
		return ErrSessionUnverified
	}
	return CheckSessionToken(secret, tok, profile, time.Now(), proc.StartTime)
}

// SessionsFile is the previous build's stored-token file, read for its bridge; it stays in that build's format.
func SessionsFile() string { return filepath.Join(paths.StateDir(), "proxy", "sessions.json") }

// LegacyRenewalsFile holds renewed expiries of previous-build tokens; only this build reads or writes it.
func LegacyRenewalsFile() string {
	return filepath.Join(paths.StateDir(), "proxy", "legacy-renewals.json")
}

// AuthenticClaims is tok's claims when its MAC verifies under this config's router secret, whatever its liveness.
func AuthenticClaims(tok string) (SessionClaims, bool) {
	secret, ok, err := vaultSecret()
	if err != nil || !ok {
		return SessionClaims{}, false
	}
	c, err := ParseSessionToken(secret, tok)
	return c, err == nil
}

// pruneProxyTokens removes cache files whose pid is gone; best effort, a stray file is harmless.
func pruneProxyTokens(dir string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		pid, err := strconv.Atoi(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil || !alive(pid) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
