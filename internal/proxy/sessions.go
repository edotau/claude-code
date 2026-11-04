package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"syscall"
	"time"
)

type SessionRequest struct {
	Profile string `json:"profile,omitempty"`
	PID     int    `json:"pid,omitempty"`
	Token   string `json:"token,omitempty"`
}

type SessionResponse struct {
	Token     string `json:"token"`
	ExpiresIn int    `json:"expires_in"`
}

// MintSessionToken asks the daemon (starting it if needed) for a token scoped to profile and bound to pid.
func MintSessionToken(ctx context.Context, profile string, pid int) (string, error) {
	var out SessionResponse
	if err := postSession(ctx, true, SessionPath, SessionRequest{Profile: profile, PID: pid}, &out); err != nil {
		return "", err
	}
	return out.Token, nil
}

// RevokeSessionToken drops token at the daemon; nil only when no daemon runs (no state, dead pid, refused port).
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
	st, err := ReadState()
	if err != nil || !alive(st.PID) {
		return ErrNotRunning
	}
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, Base(st.Port)+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if errors.Is(err, syscall.ECONNREFUSED) { // nothing listens: the recorded pid is not a live proxy
		return ErrNotRunning
	}
	if err != nil {
		return fmt.Errorf("proxy pid %d unreachable on port %d: %w", st.PID, st.Port, err)
	}
	_ = resp.Body.Close()
	if got := resp.Header.Get(PIDHeader); resp.StatusCode != http.StatusOK || got != strconv.Itoa(st.PID) {
		return fmt.Errorf("port %d answers %s as pid %q, not proxy pid %d; secret withheld", st.Port, resp.Status, got, st.PID)
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
