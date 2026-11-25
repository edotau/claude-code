package memory

import (
	"net/http"

	"github.com/edotau/claude-code/internal/httpx"
)

// embedClient is the default client with a same-origin redirect policy, so the credential never leaves its host.
func embedClient() *http.Client {
	c := *http.DefaultClient
	c.CheckRedirect = httpx.SameOriginRedirect
	return &c
}
