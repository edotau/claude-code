package providers

import (
	"bytes"
	"errors"
	"net/http"

	"github.com/edotau/claude-code/internal/databricks/sdkport"
)

// AuthVerdict is what an auth failure means for routing: only needs_login and rejected condemn the account.
type AuthVerdict string

const (
	VerdictOK         AuthVerdict = "ok"
	VerdictNeedsLogin AuthVerdict = "needs_login"
	VerdictRejected   AuthVerdict = "rejected"
	VerdictTransient  AuthVerdict = "transient"
)

// Verdict classifies a mint/refresh error; transport, timeout, 429 and 5xx are transient and never condemn.
func Verdict(err error) AuthVerdict {
	switch {
	case err == nil:
		return VerdictOK
	case errors.Is(err, ErrNeedsLogin) || sdkport.IsLoginRequired(err):
		return VerdictNeedsLogin
	case sdkport.IsRejected(err):
		return VerdictRejected
	}
	return VerdictTransient
}

// Condemns reports whether the verdict should mark the account unusable until a login or credential change.
func (v AuthVerdict) Condemns() bool { return v == VerdictNeedsLogin || v == VerdictRejected }

// IsInvalidTokenResponse reports the AI Gateway's 400 "Invalid Token" on a Databricks row (a ≤256-byte body peek).
func IsInvalidTokenResponse(p *Provider, status int, body []byte) bool {
	if p == nil || (p.Preset != PresetDatabricks && !sealedAuth(p.Auth.AuthType)) {
		return false
	}
	return status == http.StatusBadRequest && bytes.Contains(body, []byte(`"Invalid Token"`))
}
