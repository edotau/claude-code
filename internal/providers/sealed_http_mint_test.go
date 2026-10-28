package providers

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestLoopbackHTTPSealedRowStillMints(t *testing.T) {
	srv := oidcServer(t, func(_ *http.Request, _ url.Values) (int, map[string]any) {
		return 200, map[string]any{"access_token": "loop-minted", "expires_in": 3600}
	})
	p := mintRow(t, "kind-loop-http", AuthM2M, "sp-client", srv.URL)
	if err := SealSecret(AuthSecretName(p), "sp-secret"); err != nil {
		t.Fatal(err)
	}
	if got, err := Credential(context.Background(), p); err != nil || got != "loop-minted" {
		t.Fatalf("Credential = %q, %v", got, err)
	}
}

func TestPlainHTTPNonLoopbackSealedRowDoesNotMint(t *testing.T) {
	p := mintRow(t, "kind-nonloop-http", AuthM2M, "sp-client", "http://example.cloud.databricks.com")
	if err := SealSecret(AuthSecretName(p), "sp-secret"); err != nil {
		t.Fatal(err)
	}
	got, err := Credential(context.Background(), p)
	if err == nil || got != "" || strings.Contains(err.Error(), "sp-secret") {
		t.Fatalf("Credential = %q, %v; want an error and no token", got, err)
	}
}
