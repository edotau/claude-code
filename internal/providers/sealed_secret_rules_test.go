package providers

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const dbxHost = "https://example.cloud.databricks.com"

func patRow(host string) string {
	return `{"preset":"databricks","host":"` + host + `","auth":{"type":"pat"}}`
}

func TestValidateRejectsCollidingSealedSecretNames(t *testing.T) {
	doc := `{"providers":{"databricks":{"a-b":` + patRow(dbxHost) + `,"a_b":` + patRow(dbxHost) + `}}}`
	err := Validate([]byte(doc))
	if err == nil || !strings.Contains(err.Error(), "a-b") || !strings.Contains(err.Error(), "a_b") {
		t.Fatalf("Validate = %v, want an error naming a-b and a_b", err)
	}
}

func TestValidateAcceptsDistinctSealedSecretNames(t *testing.T) {
	doc := `{"providers":{"databricks":{"a-b":` + patRow(dbxHost) + `,"c_d":` + patRow(dbxHost) + `}}}`
	if err := Validate([]byte(doc)); err != nil {
		t.Fatalf("Validate = %v", err)
	}
}

func m2mBaseRow(base string) string {
	return `{"protocol":"anthropic","base_url":"` + base + `/serving-endpoints/anthropic",` +
		`"auth":{"type":"m2m","clientId":"sp-client"}}`
}

func TestValidateRejectsPlainHTTPSealedHost(t *testing.T) {
	for _, doc := range []string{
		`{"providers":{"databricks":{"m2m-http":` + m2mBaseRow("http://example.cloud.databricks.com") + `}}}`,
		`{"providers":{"databricks":{"pat-http":{"preset":"databricks","host":"http://example.cloud.databricks.com","auth":{"type":"pat"}}}}}`,
	} {
		if err := Validate([]byte(doc)); err == nil {
			t.Errorf("Validate accepted plain http: %s", doc)
		}
	}
}

func TestValidateAcceptsHTTPSAndLoopbackHTTPSealedHost(t *testing.T) {
	for _, base := range []string{dbxHost, "http://127.0.0.1:8080", "http://localhost:9"} {
		doc := `{"providers":{"databricks":{"m2m-ok":` + m2mBaseRow(base) + `}}}`
		if err := Validate([]byte(doc)); err != nil {
			t.Errorf("Validate(%s) = %v", base, err)
		}
	}
}

func TestAuthHostRejectsPlainHTTPNonLoopback(t *testing.T) {
	p := &Provider{Name: "x", BaseURL: "http://example.cloud.databricks.com/serving-endpoints/anthropic",
		Auth: Auth{Type: AuthM2M, ClientID: "c"}}
	if h := AuthHost(p); h != "" {
		t.Errorf("AuthHost = %q, want empty for plain http non-loopback", h)
	}
}

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
