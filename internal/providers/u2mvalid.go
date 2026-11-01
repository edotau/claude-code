package providers

import (
	"context"
	"time"
)

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
