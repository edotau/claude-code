package gateway

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

	"github.com/edotau/claude-code/internal/paths"
)

type SessionRequest struct {
	Profiles []string `json:"profiles,omitempty"`
	PID      int      `json:"pid,omitempty"`
	Token    string   `json:"token,omitempty"`
}

type SessionResponse struct {
	Token     string `json:"token"`
	ExpiresIn int    `json:"expires_in"`
}

// MintScopedToken asks the daemon (starting it if needed) for a token covering profiles and bound to pid.
func MintScopedToken(ctx context.Context, profiles []string, pid int) (string, error) {
	var out SessionResponse
	if err := postSession(ctx, true, SessionPath, SessionRequest{Profiles: profiles, PID: pid}, &out); err != nil {
		return "", err
	}
	return out.Token, nil
}

// RevokeSessionToken drops token at the daemon; nil when revoked or no daemon runs (no state, dead pid, refused port).
func RevokeSessionToken(ctx context.Context, token string) error {
	err := postSession(ctx, false, SessionRevokePath, SessionRequest{Token: token}, nil)
	if errors.Is(err, ErrNotRunning) {
		return nil
	}
	return err
}

func postSession(ctx context.Context, start bool, path string, body SessionRequest, out *SessionResponse) error {
	if start {
		if _, err := Ensure(ctx); err != nil {
			return err
		}
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

// sendSession POSTs body with the router secret to url; a non-2xx answer is an error naming path.
func sendSession(ctx context.Context, url, path, secret string, body SessionRequest, out *SessionResponse) error {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
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

// SessionPath and SessionRevokePath are POST endpoints requiring the router secret.
const (
	SessionPath       = "/internal/session"
	SessionRevokePath = "/internal/session/revoke"
)

var (
	tokenEnsure = Ensure
	tokenState  = readLiveState
	tokenMint   = mintWithState
)

// Token ensures the daemon is running and mints a profile-scoped token bound to its PID in two health probes.
func Token(ctx context.Context, profile string) (string, error) {
	if _, err := tokenEnsure(ctx); err != nil {
		// keep the cause: vault, spawn, loopback
		return "", fmt.Errorf("proxy unavailable (%v); run gateway proxy --start", err)
	}
	st, err := tokenState()
	if errors.Is(err, ErrNotRunning) {
		return "", errors.New("proxy not running")
	}
	if err != nil {
		return "", err
	}
	return tokenMint(ctx, st, profile)
}

func mintWithState(ctx context.Context, st State, profile string) (string, error) {
	var out SessionResponse
	if err := postWithState(ctx, st, SessionPath, SessionRequest{Profiles: []string{profile}, PID: st.PID}, &out); err != nil {
		return "", err
	}
	return out.Token, nil
}

// proxyTokenMaxAge re-mints inside the router's 12h idle lifetime, as agent.SessionToken does.
const proxyTokenMaxAge = time.Hour

// mintScoped is the mint seam of ResolveProxyToken.
var mintScoped = MintScopedToken

type proxyTokenRecord struct {
	Token    string    `json:"token"`
	Scope    []string  `json:"scope"`
	MintedAt time.Time `json:"minted_at"`
}

func proxyTokenDir() string { return filepath.Join(paths.StateDir(), "proxy-tokens") }

// ResolveProxyToken returns pid's scoped session token: cached (0600) while fresh and same-scope, else minted; never the secret.
func ResolveProxyToken(ctx context.Context, scope []string, pid int) (string, error) {
	dir := proxyTokenDir()
	pruneProxyTokens(dir)
	file := filepath.Join(dir, strconv.Itoa(pid)+".json")
	if b, err := os.ReadFile(file); err == nil {
		var rec proxyTokenRecord
		if json.Unmarshal(b, &rec) == nil && rec.Token != "" && slices.Equal(rec.Scope, scope) &&
			time.Since(rec.MintedAt) < proxyTokenMaxAge {
			return rec.Token, nil
		}
	}
	tok, err := mintScoped(ctx, scope, pid)
	if err != nil {
		return "", fmt.Errorf("mint scoped token for profiles %s: %w", strings.Join(scope, ","), err)
	}
	b, err := json.Marshal(proxyTokenRecord{Token: tok, Scope: scope, MintedAt: time.Now()})
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
