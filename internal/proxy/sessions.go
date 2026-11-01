package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

// RevokeSessionToken drops token at the daemon; if it is not running, no request can use the token.
func RevokeSessionToken(ctx context.Context, token string) error {
	err := postSession(ctx, false, SessionRevokePath, SessionRequest{Token: token}, nil)
	if errors.Is(err, ErrNotRunning) {
		return nil
	}
	return err
}

func postSession(ctx context.Context, start bool, path string, body SessionRequest, out *SessionResponse) error {
	var base string
	if start {
		b, err := Ensure(ctx)
		if err != nil {
			return err
		}
		base = b
	} else {
		st, ok := Running(ctx)
		if !ok {
			return ErrNotRunning
		}
		base = Base(st.Port)
	}
	secret, err := ClientSecret()
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(raw))
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

// SessionPath and SessionRevokePath are POST endpoints requiring the router secret.
const (
	SessionPath       = "/internal/session"
	SessionRevokePath = "/internal/session/revoke"
)
