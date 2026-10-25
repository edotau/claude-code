package providers

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
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

// jwtExp returns the numeric exp claim of a JWT-shaped credential; false for anything else.
func jwtExp(v string) (int64, bool) {
	var claims struct {
		Exp *float64 `json:"exp"`
	}
	if !jwtClaims(v, &claims) || claims.Exp == nil {
		return 0, false
	}
	return int64(*claims.Exp), true
}

// jwtClaims decodes the payload of a three-part JWT-shaped string into into; false for anything else.
func jwtClaims(v string, into any) bool {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	return err == nil && json.Unmarshal(payload, into) == nil
}

// recordTokenExpiry stores only the exp claim of a JWT credential; every error is ignored.
func recordTokenExpiry(name, v string) {
	exp, ok := jwtExp(v)
	path := tokenExpiryPath(name)
	if !ok || path == "" {
		return
	}
	b, err := json.Marshal(map[string]int64{"exp": exp})
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	_ = os.WriteFile(path, b, 0o600)
}

// TokenExpiry is the expiry a provider's JWT credential last reported; false when none is recorded.
func TokenExpiry(name string) (time.Time, bool) {
	path := tokenExpiryPath(name)
	if path == "" {
		return time.Time{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, false
	}
	var rec struct {
		Exp *int64 `json:"exp"`
	}
	if json.Unmarshal(b, &rec) != nil || rec.Exp == nil {
		return time.Time{}, false
	}
	return time.Unix(*rec.Exp, 0), true
}
