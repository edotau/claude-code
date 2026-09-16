package providers

import (
	"context"
	"errors"
	"net/http"
)

// Authorize sets the provider's auth header and static headers on h. Passthrough leaves client auth untouched.
func Authorize(ctx context.Context, p *Provider, h http.Header) error {
	for k, v := range p.Headers {
		h.Set(k, v)
	}
	cred, err := Credential(ctx, p)
	if errors.Is(err, ErrPassthrough) {
		return nil
	}
	if err != nil {
		return err
	}
	h.Del("Authorization")
	h.Del("X-Api-Key")
	switch p.Auth.Type {
	case AuthAPIKey:
		h.Set("X-Api-Key", cred)
	case AuthBearer:
		h.Set("Authorization", "Bearer "+cred)
	}
	return nil
}
