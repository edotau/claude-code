package providers

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
)

// tokenNameRE admits only a provider name that is a safe single path segment.
var tokenNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// tokenExpiryPath is the expiry record for a provider; "" when the name is not a safe path segment.
func tokenExpiryPath(name string) string {
	if !tokenNameRE.MatchString(name) || name == "." || name == ".." {
		return ""
	}
	return filepath.Join(paths.StateDir(), "cache", "token-expiry", name+".json")
}

// maxJWTExp bounds a plausible exp claim (year 9999) so the float64 to int64 conversion cannot overflow.
const maxJWTExp = 253402300799

// JWTExp returns the numeric exp claim of a JWT-shaped credential; false for anything else.
func JWTExp(v string) (int64, bool) {
	var claims struct {
		Exp *float64 `json:"exp"`
	}
	if !textutil.JWTClaims(v, &claims) || claims.Exp == nil {
		return 0, false
	}
	if e := *claims.Exp; math.IsNaN(e) || e < 0 || e > maxJWTExp {
		return 0, false
	}
	return int64(*claims.Exp), true
}

// tokenExpiryRecord is the on-disk expiry record; Exp is a pointer so an absent claim stays distinguishable.
type tokenExpiryRecord struct {
	Schema int    `json:"schema"`
	Exp    *int64 `json:"exp"`
	Method string `json:"method,omitempty"` // statusline vocabulary of the minting credential: u2m, m2m, pat, ...
}

// recordTokenExpiry stores only the exp claim of a JWT credential and the method that minted it; errors are ignored.
func recordTokenExpiry(name, v, method string) {
	exp, ok := JWTExp(v)
	path := tokenExpiryPath(name)
	if !ok || path == "" {
		return
	}
	b, err := json.Marshal(tokenExpiryRecord{Schema: tokenExpirySchema, Exp: &exp, Method: method})
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	_ = paths.AtomicWrite(path, b, 0o600) // temp+rename: a concurrent TokenExpiry never reads a truncated file
}

// spExpirySuffix keys a paired row's service-principal expiry record apart from its user login's (name + suffix).
const spExpirySuffix = ".sp"

// TokenExpiry is the expiry the record name holds (a row's user or own credential; name+spExpirySuffix its SP's).
func TokenExpiry(name string) (time.Time, bool) {
	exp, _, _, ok := readTokenExpiry(name)
	return exp, ok
}

// TokenExpiryMethod is the row's latest-minted expiry, user or SP record, plus its method ("" when it names none).
func TokenExpiryMethod(name string) (time.Time, string, bool) {
	exp, method, at, ok := readTokenExpiry(name)
	if spExp, spMethod, spAt, spOK := readTokenExpiry(name + spExpirySuffix); spOK && (!ok || spAt.After(at)) {
		return spExp, spMethod, true
	}
	return exp, method, ok
}

// readTokenExpiry reads one expiry record and when it was written; false when absent or unreadable.
func readTokenExpiry(name string) (exp time.Time, method string, written time.Time, ok bool) {
	path := tokenExpiryPath(name)
	if path == "" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var rec tokenExpiryRecord
	if !schemas.DecodeOK(tokenExpiryFormat, b, &rec) || rec.Exp == nil {
		return
	}
	return time.Unix(*rec.Exp, 0), rec.Method, info.ModTime(), true
}

// expiryMethod maps an auth method to the statusline's credential label: a user OAuth login is u2m.
func expiryMethod(m string) string {
	if m == AuthOAuth {
		return "u2m"
	}
	return m
}

// tokenExpirySchema is the token-expiry record's schema version; a mismatch is a cache miss.
const tokenExpirySchema = 2

var tokenExpiryFormat = schemas.Register(schemas.Format{
	Name: "token-expiry", Owner: "internal/providers", Kind: schemas.Cache,
	Glob:    func() string { return filepath.Join(paths.StateDir(), "cache", "token-expiry", "*.json") },
	New:     func() any { return &tokenExpiryRecord{} },
	Version: tokenExpirySchema, Keep: "rebuildable cache; a version mismatch is a miss",
})

// ValidU2M reports the remaining life of a u2m login that needs no browser: a cached future expiry, else one refresh.
func ValidU2M(ctx context.Context, p *Provider) (time.Duration, bool) {
	if p.Auth.Method() != AuthOAuth {
		return 0, false
	}
	if secret, err := mintSecret(p); err != nil || secret == "" {
		return 0, false
	}
	if _, err := userCredential(ctx, p, false); err != nil {
		return 0, false
	}
	exp, ok := TokenExpiry(p.Name)
	if !ok || time.Until(exp) <= 0 {
		return 0, false
	}
	return time.Until(exp), true
}
